package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

type Server struct {
	http     *http.Server
	listener net.Listener
	logger   *slog.Logger
}

// New binds a listener on addr and prepares the HTTP server. Binding here rather
// than inside Run has two consequences worth the extra error return: a port
// already in use fails immediately at startup instead of racing the rest of the
// boot sequence, and Addr reports the real port when addr ends in ":0" — which
// is how the e2e suite runs a server per test without a port registry.
func New(addr string, handler http.Handler, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	return &Server{
		http: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      15 * time.Second,
		},
		listener: listener,
		logger:   logger,
	}, nil
}

// Addr returns the address actually bound, including the kernel-assigned port
// when the configured address used port 0.
func (s *Server) Addr() string { return s.listener.Addr().String() }

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
		s.logger.Info("server shutdown requested", "reason", ctx.Err())
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return err
	}

	return <-errCh
}
