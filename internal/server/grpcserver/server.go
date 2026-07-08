package grpcserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"metrics-system/internal/auth"
	metricspb "metrics-system/internal/proto/metricspb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

// gracefulStopTimeout bounds how long we wait for in-flight RPCs to finish
// before forcing the server down.
const gracefulStopTimeout = 10 * time.Second

// Server owns a gRPC server and its listener, with a lifecycle that mirrors the
// HTTP server: Run blocks until ctx is cancelled, then drains gracefully.
type Server struct {
	grpc   *grpc.Server
	lis    net.Listener
	logger *slog.Logger
}

// New binds a listener on addr and registers the MetricsService with recovery
// and logging interceptors plus server reflection (for grpcurl). When
// authenticator is non-nil, auth/RBAC interceptors are added (innermost, so the
// logging interceptor still records rejected calls).
func New(addr string, svc metricspb.MetricsServiceServer, authenticator auth.Authenticator, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	// recover (outermost) -> log -> [auth] -> handler.
	unary := []grpc.UnaryServerInterceptor{recoverUnary(logger), logUnary(logger)}
	stream := []grpc.StreamServerInterceptor{recoverStream(logger), logStream(logger)}
	if authenticator != nil {
		unary = append(unary, authUnary(authenticator))
		stream = append(stream, authStream(authenticator))
	}

	gs := grpc.NewServer(
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(stream...),
	)
	metricspb.RegisterMetricsServiceServer(gs, svc)
	reflection.Register(gs)
	return &Server{grpc: gs, lis: lis, logger: logger}, nil
}

// Addr returns the actual bound address (useful when addr was ":0").
func (s *Server) Addr() string { return s.lis.Addr().String() }

// Run serves until ctx is cancelled or Serve fails, then stops gracefully,
// falling back to a hard stop if draining exceeds gracefulStopTimeout.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		// Serve returns nil once GracefulStop/Stop is called.
		err := s.grpc.Serve(s.lis)
		if errors.Is(err, grpc.ErrServerStopped) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("grpc server shutdown requested", "reason", ctx.Err())
	case err := <-errCh:
		// Serve failed on its own (an abnormal exit that leaves accepted
		// streams running). Stop blocks until every handler returns, so callers
		// can rely on "no handler runs after Run returns" on every path.
		s.grpc.Stop()
		return err
	}

	stopped := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(gracefulStopTimeout):
		s.logger.Warn("grpc graceful stop timed out; forcing stop")
		s.grpc.Stop()
		<-stopped
	}

	return <-errCh
}
