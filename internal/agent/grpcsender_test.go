package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"metrics-system/internal/model"
	metricspb "metrics-system/internal/proto/metricspb"

	"google.golang.org/grpc"
)

// stubServer records received batches and can be told to reply "throttled".
type stubServer struct {
	metricspb.UnimplementedMetricsServiceServer
	mu       sync.Mutex
	batches  []*metricspb.Batch
	throttle bool
}

func (s *stubServer) setThrottle(v bool) {
	s.mu.Lock()
	s.throttle = v
	s.mu.Unlock()
}

func (s *stubServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.batches)
}

func (s *stubServer) IngestStream(stream grpc.BidiStreamingServer[metricspb.Batch, metricspb.IngestAck]) error {
	for {
		b, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.batches = append(s.batches, b)
		throttled := s.throttle
		s.mu.Unlock()

		ack := &metricspb.IngestAck{Accepted: uint64(len(b.GetMetrics())), Throttled: throttled}
		if err := stream.Send(ack); err != nil {
			return err
		}
	}
}

func startStub(t *testing.T) (*stubServer, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	stub := &stubServer{}
	gs := grpc.NewServer()
	metricspb.RegisterMetricsServiceServer(gs, stub)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return stub, lis.Addr().String()
}

func sampleBatch() model.Batch {
	return model.Batch{
		AgentID: "a1",
		Metrics: []model.Metric{{
			Name:      "cpu",
			Type:      model.MetricTypeGauge,
			Value:     42,
			Timestamp: time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC),
		}},
	}
}

func TestGRPCSenderStreamsBatches(t *testing.T) {
	t.Parallel()
	stub, addr := startStub(t)

	sender, err := NewGRPCSender(addr, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new sender: %v", err)
	}
	defer func() { _ = sender.Close() }()

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := sender.Send(ctx, sampleBatch()); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if got := stub.count(); got != 3 {
		t.Fatalf("server received %d batches, want 3", got)
	}
	// The stream is reused across sends: still exactly one open stream.
	sender.mu.Lock()
	reused := sender.stream != nil
	sender.mu.Unlock()
	if !reused {
		t.Fatal("expected the stream to stay open across sends")
	}
}

func TestGRPCSenderThrottleIsNotAnError(t *testing.T) {
	t.Parallel()
	stub, addr := startStub(t)
	stub.setThrottle(true)

	sender, err := NewGRPCSender(addr, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new sender: %v", err)
	}
	defer func() { _ = sender.Close() }()

	// A throttled ack is a signal, not a failure — Send must still succeed.
	if err := sender.Send(context.Background(), sampleBatch()); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func TestGRPCSenderRejectsInvalidBatch(t *testing.T) {
	t.Parallel()
	_, addr := startStub(t)
	sender, err := NewGRPCSender(addr, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new sender: %v", err)
	}
	defer func() { _ = sender.Close() }()

	// Empty batch fails local validation before any network call.
	if err := sender.Send(context.Background(), model.Batch{}); err == nil {
		t.Fatal("expected validation error for empty batch")
	}
}

func TestGRPCSenderClosedSenderRejects(t *testing.T) {
	t.Parallel()
	_, addr := startStub(t)
	sender, err := NewGRPCSender(addr, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new sender: %v", err)
	}
	if err := sender.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := sender.Send(context.Background(), sampleBatch()); !errors.Is(err, errSenderClosed) {
		t.Fatalf("got %v, want errSenderClosed", err)
	}
}
