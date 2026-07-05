package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"metrics-system/internal/grpcconv"
	"metrics-system/internal/model"
	metricspb "metrics-system/internal/proto/metricspb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// errSenderClosed is returned by Send after Close.
var errSenderClosed = errors.New("grpc sender closed")

// GRPCSender ships batches over a single, long-lived bidirectional stream that
// is reused across ticks (much cheaper than a fresh RPC per batch). It drives
// the stream in lockstep — send one batch, wait for its ack — which keeps the
// per-tick Transport contract intact and means no batch is ever left unacked.
// A broken stream is transparently reopened on the next Send.
type GRPCSender struct {
	conn   *grpc.ClientConn
	client metricspb.MetricsServiceClient
	logger *slog.Logger

	mu           sync.Mutex
	stream       metricspb.MetricsService_IngestStreamClient
	streamCancel context.CancelFunc
	closed       bool
}

// NewGRPCSender dials target (a host:port, not a URL) with an insecure
// connection. The dial is lazy; the first Send establishes the stream.
func NewGRPCSender(target string, logger *slog.Logger) (*GRPCSender, error) {
	if logger == nil {
		logger = slog.Default()
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial %q: %w", target, err)
	}
	return &GRPCSender{
		conn:   conn,
		client: metricspb.NewMetricsServiceClient(conn),
		logger: logger,
	}, nil
}

type ackResult struct {
	ack *metricspb.IngestAck
	err error
}

// Send streams one batch and waits for its ack, without blocking past ctx.
func (s *GRPCSender) Send(ctx context.Context, batch model.Batch) error {
	if err := batch.Validate(); err != nil {
		return fmt.Errorf("invalid batch: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errSenderClosed
	}

	stream, err := s.ensureStreamLocked()
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}
	if err := stream.Send(grpcconv.BatchToProto(batch)); err != nil {
		s.resetStreamLocked()
		return fmt.Errorf("stream send: %w", err)
	}

	// Receive the ack in a goroutine so a stalled server can't outlast ctx.
	// resetStreamLocked cancels the stream context, which unblocks Recv.
	ch := make(chan ackResult, 1)
	go func() {
		ack, err := stream.Recv()
		ch <- ackResult{ack: ack, err: err}
	}()

	select {
	case <-ctx.Done():
		s.resetStreamLocked()
		return ctx.Err()
	case r := <-ch:
		if r.err != nil {
			s.resetStreamLocked()
			return fmt.Errorf("recv ack: %w", r.err)
		}
		if r.ack.GetThrottled() {
			s.logger.Warn("server throttled batch", "agent_id", batch.AgentID, "metrics", len(batch.Metrics))
		}
		return nil
	}
}

// ensureStreamLocked returns the current stream, opening a new one (bound to its
// own cancelable, background-rooted context) if there is none. Caller holds mu.
func (s *GRPCSender) ensureStreamLocked() (metricspb.MetricsService_IngestStreamClient, error) {
	if s.stream != nil {
		return s.stream, nil
	}
	// The stream must outlive individual ticks, so it is rooted in a background
	// context we own and cancel on reset/close — not the per-tick ctx.
	streamCtx, cancel := context.WithCancel(context.Background())
	stream, err := s.client.IngestStream(streamCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	s.stream = stream
	s.streamCancel = cancel
	return stream, nil
}

// resetStreamLocked tears down the current stream so the next Send reopens one.
// Cancelling the stream context unblocks any in-flight Recv. Caller holds mu.
func (s *GRPCSender) resetStreamLocked() {
	if s.streamCancel != nil {
		s.streamCancel()
		s.streamCancel = nil
	}
	s.stream = nil
}

// Close half-closes the stream and shuts the connection down. Because Send runs
// in lockstep, no batch is outstanding at this point.
func (s *GRPCSender) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.stream != nil {
		_ = s.stream.CloseSend()
	}
	s.resetStreamLocked()
	return s.conn.Close()
}
