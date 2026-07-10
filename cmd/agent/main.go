package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"metrics-system/internal/agent"
	"metrics-system/internal/agent/kernel"
	"metrics-system/internal/agent/network"
	"metrics-system/pkg/httpx"
)

func main() {
	hostname, _ := os.Hostname()

	defaultServer := envString("AGENT_SERVER", "http://localhost:8080/api/v1/metrics")
	defaultTransport := envString("AGENT_TRANSPORT", "http")
	defaultGRPCServer := envString("AGENT_GRPC_SERVER", "localhost:9090")
	defaultAPIKey := envString("AGENT_API_KEY", "")
	defaultAuthToken := envString("AGENT_AUTH_TOKEN", "")
	defaultInterval := envDuration("AGENT_INTERVAL", 5*time.Second)
	defaultID := envString("AGENT_ID", hostname)
	defaultDiskPath := envString("AGENT_DISK_PATH", "/")
	defaultTimeout := envDuration("AGENT_HTTP_TIMEOUT", 10*time.Second)
	defaultRetries := envInt("AGENT_HTTP_RETRIES", 2)
	defaultBackoff := envDuration("AGENT_HTTP_BACKOFF", 200*time.Millisecond)
	defaultLevel := envString("AGENT_LOG_LEVEL", "info")

	var (
		serverURL     = flag.String("server", defaultServer, "HTTP server endpoint (for -transport=http)")
		transportName = flag.String("transport", defaultTransport, "transport: http|grpc")
		grpcServer    = flag.String("grpc-server", defaultGRPCServer, "gRPC server target host:port (for -transport=grpc)")
		interval      = flag.Duration("interval", defaultInterval, "collection interval")
		agentID       = flag.String("id", defaultID, "agent id")
		diskPath      = flag.String("disk-path", defaultDiskPath, "disk path for usage metrics")
		httpTO        = flag.Duration("http-timeout", defaultTimeout, "http timeout")
		httpRetry     = flag.Int("http-retries", defaultRetries, "http retry count")
		httpBackof    = flag.Duration("http-backoff", defaultBackoff, "http retry base backoff")
		apiKey        = flag.String("api-key", defaultAPIKey, "API key to authenticate to the server")
		authToken     = flag.String("auth-token", defaultAuthToken, "bearer (JWT) token to authenticate to the server")
		logLevel      = flag.String("log-level", defaultLevel, "log level: debug|info|warn|error")

		netEnabled = flag.Bool("network", envBool("AGENT_NETWORK", false), "capture packets with libpcap and report network metrics (needs CGo and privileges)")
		netDevice  = flag.String("network-device", envString("AGENT_NETWORK_DEVICE", ""), "interface to capture on (en0, eth0, any)")
		netFile    = flag.String("network-file", envString("AGENT_NETWORK_FILE", ""), "read packets from a .pcap savefile instead of an interface")
		netFilter  = flag.String("network-filter", envString("AGENT_NETWORK_FILTER", "ip or ip6"), "BPF filter applied in the kernel (tcpdump syntax)")
		netSnapLen = flag.Int("network-snaplen", envInt("AGENT_NETWORK_SNAPLEN", 128), "bytes captured per packet; the headers are all that is classified")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(*logLevel)}))

	collectors := []agent.Collector{
		agent.NewCPUCollector(hostname),
		agent.NewMemoryCollector(hostname),
		agent.NewDiskCollector(hostname, *diskPath),
		agent.NewUptimeCollector(hostname),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Kernel network counters, read from /proc/net. No CGo, no privileges, no
	// dependency — which is why this is attempted before packet capture and not
	// gated behind a flag. On a host without /proc it simply is not there.
	if kc, err := kernel.NewCollector(hostname); err != nil {
		logger.Debug("kernel collector unavailable", "error", err)
	} else {
		collectors = append(collectors, kc)
		logger.Info("kernel collector enabled", "source", "/proc/net/{snmp,netstat}")
	}

	// Packet capture is opt-in, and its failures are never fatal.
	//
	// It needs three things the other collectors do not: CGo compiled in,
	// libpcap on the host, and privileges (/dev/bpf* is root-only on macOS,
	// CAP_NET_RAW on Linux). An agent that refuses to start because it could not
	// open a raw socket reports nothing at all — which is strictly worse than an
	// agent that reports CPU, memory and disk while saying, once, why it has no
	// network metrics.
	if *netEnabled {
		if nc := startNetworkCollector(ctx, networkOptions{
			device:  *netDevice,
			file:    *netFile,
			filter:  *netFilter,
			snapLen: *netSnapLen,
		}, logger); nc != nil {
			collectors = append(collectors, nc)
			defer func() { _ = nc.Close() }()
		}
	}

	creds := agent.Credentials{APIKey: *apiKey, Bearer: *authToken}
	transport, err := buildTransport(*transportName, *serverURL, *grpcServer, *httpTO, *httpRetry, *httpBackof, creds, logger)
	if err != nil {
		logger.Error("build transport failed", "transport", *transportName, "error", err)
		os.Exit(1)
	}
	a := agent.New(*agentID, *interval, collectors, transport, logger)

	if err := a.Run(ctx); err != nil {
		logger.Error("agent terminated", "error", err)
		os.Exit(1)
	}
}

// networkOptions is the flag surface of the packet-capture collector.
type networkOptions struct {
	device  string
	file    string
	filter  string
	snapLen int
}

// startNetworkCollector opens the capture and starts its background read loop.
// It returns nil — after logging why — whenever capture is unavailable: built
// without CGo, no libpcap, no permission, no such interface. The agent runs on.
func startNetworkCollector(ctx context.Context, opts networkOptions, logger *slog.Logger) *network.Collector {
	if !network.Available() {
		logger.Warn("network collector disabled", "reason", network.ErrUnsupported)
		return nil
	}
	if opts.device == "" && opts.file == "" {
		logger.Warn("network collector disabled", "reason", "set -network-device or -network-file")
		return nil
	}

	nc, err := network.NewCollector(network.Config{
		Device:  opts.device,
		File:    opts.file,
		Filter:  opts.filter,
		SnapLen: opts.snapLen,
	}, logger)
	if err != nil {
		logger.Warn("network collector disabled", "device", opts.device, "file", opts.file, "error", err)
		return nil
	}

	logger.Info("network collector enabled",
		"device", opts.device, "file", opts.file, "filter", opts.filter,
		"link_type", nc.LinkType().String(), "libpcap", network.LibraryVersion())

	go nc.Run(ctx)
	return nc
}

// buildTransport constructs the agent's Transport from the -transport choice.
func buildTransport(name, serverURL, grpcTarget string, httpTO time.Duration, retries int, backoff time.Duration, creds agent.Credentials, logger *slog.Logger) (agent.Transport, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "http":
		client := httpx.NewClient(httpTO, retries, backoff)
		logger.Info("using HTTP transport", "server", serverURL)
		return agent.NewSender(serverURL, client, creds), nil
	case "grpc":
		logger.Info("using gRPC transport", "server", grpcTarget)
		return agent.NewGRPCSender(grpcTarget, creds, logger)
	default:
		return nil, fmt.Errorf("unknown transport %q (want http|grpc)", name)
	}
}

func parseLogLevel(v string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func envString(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
