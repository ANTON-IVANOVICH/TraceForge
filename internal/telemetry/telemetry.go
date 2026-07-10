// Package telemetry serves the admin surface every TraceForge binary shares: the
// three Kubernetes probes and the Prometheus scrape.
//
// It is a listener of its own rather than three more routes on the API mux, for
// three reasons that only show up in production:
//
//   - /metrics names the pipeline's failure counts, the alert severities that are
//     firing and the process's memory layout. It belongs on a port a NetworkPolicy
//     can close and an ingress never sees.
//   - The API listener has timeouts tuned for API requests and, when auth is on,
//     an authenticator in front of it. A probe must answer the kubelet whether or
//     not the operator configured a JWKS URL, and adding "/readyz" to the auth
//     exemption list is one refactor away from being forgotten.
//   - A saturated API listener — every connection consumed by slow ingests — is
//     precisely when the readiness probe most needs to answer. Sharing the listener
//     means the probe queues behind the problem it exists to report.
//
// It is also its own package rather than part of internal/server, because the
// agent needs it too, and an agent that imported internal/server would link the
// storage engine, the alerting subsystem and the embedded dashboard into a binary
// that runs on every node.
//
// pprof stays on a third listener still. It is a different kind of surface:
// /debug/pprof/cmdline prints this process's argv, and argv is where
// -jwt-hs256-secret lives.
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"metrics-system/internal/promexport"
	"metrics-system/internal/server/health"
)

// Timeouts for the admin listener. They are shorter than the API's on purpose: a
// probe or a scrape that takes longer than a couple of seconds has already failed
// as far as its caller is concerned, and holding the connection open only ties up
// the goroutine that would answer the next one.
const (
	readHeaderTimeout = 2 * time.Second
	readTimeout       = 5 * time.Second
	writeTimeout      = 10 * time.Second

	// shutdownGrace bounds the wait for in-flight scrapes when the listener is
	// told to stop. It is short because there is nothing here worth waiting for.
	shutdownGrace = 3 * time.Second
)

// Config is what a binary hands the telemetry listener.
type Config struct {
	// Addr is the listen address. Empty means "do not serve".
	Addr string
	// Health serves /healthz, /readyz and /startupz. Nil omits them.
	Health *health.Checker
	// Gatherers are rendered at /metrics, in order. Empty omits the endpoint.
	Gatherers []promexport.Gatherer
}

// Handler builds the admin mux.
//
// No Recover, no request id, no rate limiter, no auth. Every middleware in the
// chain is one more thing that can wedge between the kubelet and the truth.
func Handler(cfg Config, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	mux := http.NewServeMux()
	if cfg.Health != nil {
		cfg.Health.Routes(mux)
	}
	if len(cfg.Gatherers) > 0 {
		mux.Handle("GET /metrics", promexport.Handler(logger, cfg.Gatherers...))
	}
	return mux
}

// Server is the telemetry listener.
type Server struct {
	http     *http.Server
	listener net.Listener
	logger   *slog.Logger
}

// New binds cfg.Addr and prepares the admin server. Binding here rather than in
// Run means a port clash fails at startup instead of racing the boot sequence,
// and Addr reports the kernel-assigned port when the address ends in ":0" — which
// is how the e2e suite runs a server per test with no port registry.
func New(cfg Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Addr == "" {
		return nil, errors.New("telemetry: no listen address")
	}

	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	return &Server{
		http: &http.Server{
			Handler:           Handler(cfg, logger),
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
		},
		listener: listener,
		logger:   logger,
	}, nil
}

// Addr returns the address actually bound.
func (s *Server) Addr() string { return s.listener.Addr().String() }

// Run serves until ctx is cancelled, then drains briefly and returns.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		err := s.http.Serve(s.listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return <-errCh
}
