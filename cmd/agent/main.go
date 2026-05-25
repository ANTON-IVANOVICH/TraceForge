package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"metrics-system/internal/agent"
	"metrics-system/pkg/httpx"
)

func main() {
	hostname, _ := os.Hostname()

	defaultServer := envString("AGENT_SERVER", "http://localhost:8080/api/v1/metrics")
	defaultInterval := envDuration("AGENT_INTERVAL", 5*time.Second)
	defaultID := envString("AGENT_ID", hostname)
	defaultDiskPath := envString("AGENT_DISK_PATH", "/")
	defaultTimeout := envDuration("AGENT_HTTP_TIMEOUT", 10*time.Second)
	defaultRetries := envInt("AGENT_HTTP_RETRIES", 2)
	defaultBackoff := envDuration("AGENT_HTTP_BACKOFF", 200*time.Millisecond)
	defaultLevel := envString("AGENT_LOG_LEVEL", "info")

	var (
		serverURL  = flag.String("server", defaultServer, "server endpoint")
		interval   = flag.Duration("interval", defaultInterval, "collection interval")
		agentID    = flag.String("id", defaultID, "agent id")
		diskPath   = flag.String("disk-path", defaultDiskPath, "disk path for usage metrics")
		httpTO     = flag.Duration("http-timeout", defaultTimeout, "http timeout")
		httpRetry  = flag.Int("http-retries", defaultRetries, "http retry count")
		httpBackof = flag.Duration("http-backoff", defaultBackoff, "http retry base backoff")
		logLevel   = flag.String("log-level", defaultLevel, "log level: debug|info|warn|error")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(*logLevel)}))

	collectors := []agent.Collector{
		agent.NewCPUCollector(hostname),
		agent.NewMemoryCollector(hostname),
		agent.NewDiskCollector(hostname, *diskPath),
		agent.NewUptimeCollector(hostname),
	}

	client := httpx.NewClient(*httpTO, *httpRetry, *httpBackof)
	sender := agent.NewSender(*serverURL, client)
	a := agent.New(*agentID, *interval, collectors, sender, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := a.Run(ctx); err != nil {
		logger.Error("agent terminated", "error", err)
		os.Exit(1)
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
