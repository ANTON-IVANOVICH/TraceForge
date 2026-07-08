package grpcserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"metrics-system/internal/model"
	metricspb "metrics-system/internal/proto/metricspb"
	"metrics-system/internal/server/pipeline"
	"metrics-system/internal/server/storage"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newRealServer stands up the full gRPC server (interceptors, reflection,
// lifecycle) backed by a real pipeline + in-memory store, and returns a
// connected client.
func newRealServer(t *testing.T) (metricspb.MetricsServiceClient, storage.Storage) {
	t.Helper()
	store := storage.NewMemoryStorage()
	logger := discardLogger()
	pipe := pipeline.New(store, pipeline.Config{}, logger)
	pipe.Start()

	svc := NewService(pipe, store, logger)
	srv, err := New("127.0.0.1:0", svc, nil, logger)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Run(ctx)
	}()

	conn, err := grpc.NewClient(srv.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	t.Cleanup(func() {
		_ = conn.Close()
		cancel()
		<-done
		pipe.Shutdown()
		_ = store.Close()
	})
	return metricspb.NewMetricsServiceClient(conn), store
}

func protoMetric(name string, v float64, ts time.Time) *metricspb.Metric {
	return &metricspb.Metric{
		Name:      name,
		Type:      metricspb.MetricType_METRIC_TYPE_GAUGE,
		Value:     v,
		Timestamp: timestamppb.New(ts),
	}
}

// waitPoints blocks until the store holds at least n points or the deadline
// passes (the pipeline stores asynchronously).
func waitPoints(t *testing.T, store storage.Storage, n int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if store.Stats().Points >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d points, have %d", n, store.Stats().Points)
}

func TestIngestUnaryAndQuery(t *testing.T) {
	t.Parallel()
	client, store := newRealServer(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	ack, err := client.Ingest(ctx, &metricspb.Batch{
		AgentId: "a1",
		Metrics: []*metricspb.Metric{
			protoMetric("cpu", 10, base),
			protoMetric("cpu", 20, base.Add(time.Second)),
		},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if ack.GetAccepted() != 2 {
		t.Fatalf("accepted: got %d want 2", ack.GetAccepted())
	}
	waitPoints(t, store, 2)

	stream, err := client.Query(ctx, &metricspb.QueryRequest{Name: "cpu"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var got []float64
	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		got = append(got, m.GetValue())
	}
	if len(got) != 2 {
		t.Fatalf("query returned %d metrics, want 2", len(got))
	}
}

func TestIngestStream(t *testing.T) {
	t.Parallel()
	client, store := newRealServer(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	stream, err := client.IngestStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	const batches = 3
	for i := 0; i < batches; i++ {
		if err := stream.Send(&metricspb.Batch{
			AgentId: "a1",
			Metrics: []*metricspb.Metric{protoMetric("mem", float64(i), base.Add(time.Duration(i)*time.Second))},
		}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		ack, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv ack %d: %v", i, err)
		}
		if ack.GetAccepted() != 1 || ack.GetThrottled() {
			t.Fatalf("ack %d: %+v", i, ack)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
	waitPoints(t, store, batches)
}

func TestIngestUnaryRejectsInvalid(t *testing.T) {
	t.Parallel()
	client, _ := newRealServer(t)

	// Missing agent id.
	_, err := client.Ingest(context.Background(), &metricspb.Batch{
		Metrics: []*metricspb.Metric{protoMetric("cpu", 1, time.Now())},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got code %v, want InvalidArgument (err=%v)", status.Code(err), err)
	}
}

// fakeIngester lets us drive the backpressure path deterministically.
type fakeIngester struct{ accept bool }

func (f fakeIngester) Ingest(model.Batch) bool { return f.accept }

func TestUnaryBackpressure(t *testing.T) {
	t.Parallel()
	svc := NewService(fakeIngester{accept: false}, nil, discardLogger())
	_, err := svc.Ingest(context.Background(), &metricspb.Batch{
		AgentId: "a1",
		Metrics: []*metricspb.Metric{protoMetric("cpu", 1, time.Now())},
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("got code %v, want ResourceExhausted", status.Code(err))
	}
}
