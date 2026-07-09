//go:build integration

package grpcserver

import (
	"context"
	"net"
	"testing"
	"time"

	"metrics-system/internal/auth"
	metricspb "metrics-system/internal/proto/metricspb"
	"metrics-system/internal/server/pipeline"
	"metrics-system/internal/server/storage"
	"metrics-system/internal/testutil"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// These run a real gRPC client through a real connection (bufconn) into the real
// service, interceptors, pipeline and storage — the transport-level interactions
// that service_test.go (which calls the service methods directly) cannot see.
// discardLogger and protoMetric are shared with service_test.go.

// newBufconnServer stands up the service over an in-memory bufconn listener and
// returns a connected client. grpcserver.New binds its own TCP listener, so the
// interceptor chain is reconstructed here to serve over bufconn while still
// exercising the real recover/log/auth interceptors.
func newBufconnServer(t *testing.T, authenticator auth.Authenticator) (metricspb.MetricsServiceClient, storage.Storage) {
	t.Helper()
	store := storage.NewMemoryStorage()
	logger := discardLogger()
	pipe := pipeline.New(store, pipeline.Config{}, logger)
	pipe.Start()

	svc := NewService(pipe, store, logger)

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

	lis := bufconn.Listen(1 << 20)
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = gs.Serve(lis)
	}()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}

	t.Cleanup(func() {
		_ = conn.Close()
		gs.Stop() // blocks until in-flight RPCs finish and handlers return
		<-serveDone
		_ = lis.Close()
		pipe.Shutdown()
		_ = store.Close()
	})
	return metricspb.NewMetricsServiceClient(conn), store
}

// grpcQueryCount drains a Query stream and returns how many metrics came back.
// It is non-fatal (any error ends the count) so it is safe inside Eventually.
func grpcQueryCount(client metricspb.MetricsServiceClient, name string) int {
	stream, err := client.Query(context.Background(), &metricspb.QueryRequest{Name: name})
	if err != nil {
		return -1
	}
	n := 0
	for {
		if _, err := stream.Recv(); err != nil {
			return n // io.EOF (clean) or a transport error both end the count
		}
		n++
	}
}

// TestGRPC_BufconnIngestQueryRoundTrip pushes a batch and then waits for it to be
// queryable back through the same storage, over a real gRPC connection.
func TestGRPC_BufconnIngestQueryRoundTrip(t *testing.T) {
	client, _ := newBufconnServer(t, nil)
	base := testutil.BaseTime

	ack, err := client.Ingest(context.Background(), &metricspb.Batch{
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
		t.Fatalf("accepted = %d, want 2", ack.GetAccepted())
	}

	testutil.Eventually(t, 3*time.Second, 20*time.Millisecond, func() bool {
		return grpcQueryCount(client, "cpu") == 2
	}, "points never became queryable over grpc")
}

// TestGRPC_AuthInterceptorRejects checks the auth interceptor's gRPC status codes
// over the wire: no credentials -> Unauthenticated, a reader ingesting ->
// PermissionDenied, a writer -> OK.
func TestGRPC_AuthInterceptorRejects(t *testing.T) {
	authn, err := auth.NewAPIKeyAuthenticator(auth.APIKeyConfig{Keys: []auth.APIKeyEntry{
		{Key: "w-key", Subject: "w", Tenant: "t", Roles: []string{"writer"}},
		{Key: "r-key", Subject: "r", Tenant: "t", Roles: []string{"reader"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	client, _ := newBufconnServer(t, authn)
	batch := &metricspb.Batch{
		AgentId: "a1",
		Metrics: []*metricspb.Metric{protoMetric("cpu", 1, testutil.BaseTime)},
	}

	if _, err := client.Ingest(context.Background(), batch); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no creds: got %v, want Unauthenticated", status.Code(err))
	}

	rctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-api-key", "r-key"))
	if _, err := client.Ingest(rctx, batch); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("reader ingest: got %v, want PermissionDenied", status.Code(err))
	}

	wctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-api-key", "w-key"))
	ack, err := client.Ingest(wctx, batch)
	if err != nil {
		t.Fatalf("writer ingest: %v", err)
	}
	if ack.GetAccepted() != 1 {
		t.Fatalf("writer accepted = %d, want 1", ack.GetAccepted())
	}
}

// TestGRPC_StreamDisconnectNoLeak opens a bidi stream, sends a batch, then
// abruptly cancels the client context mid-stream. The server-side handler must
// observe the disconnect, return, and leave no goroutine behind. NoLeaks is
// registered first so its check runs after the server/client are torn down.
func TestGRPC_StreamDisconnectNoLeak(t *testing.T) {
	testutil.NoLeaks(t)

	client, _ := newBufconnServer(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.IngestStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(&metricspb.Batch{
		AgentId: "a1",
		Metrics: []*metricspb.Metric{protoMetric("mem", 1, testutil.BaseTime)},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv ack: %v", err)
	}

	cancel() // disconnect mid-stream; the server handler must unwind cleanly
}
