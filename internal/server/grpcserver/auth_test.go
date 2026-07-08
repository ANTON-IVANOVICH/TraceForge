package grpcserver

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"metrics-system/internal/auth"
	metricspb "metrics-system/internal/proto/metricspb"
	"metrics-system/internal/server/pipeline"
	"metrics-system/internal/server/storage"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func newAuthedGRPC(t *testing.T) (metricspb.MetricsServiceClient, storage.Storage) {
	t.Helper()
	store := storage.NewMemoryStorage()
	logger := discardLogger()
	pipe := pipeline.New(store, pipeline.Config{}, logger)
	pipe.Start()

	authn, err := auth.NewAPIKeyAuthenticator(auth.APIKeyConfig{Keys: []auth.APIKeyEntry{
		{Key: "a-writer", Subject: "wa", Tenant: "tenant-a", Roles: []string{"writer"}},
		{Key: "a-reader", Subject: "ra", Tenant: "tenant-a", Roles: []string{"reader"}},
		{Key: "b-reader", Subject: "rb", Tenant: "tenant-b", Roles: []string{"reader"}},
	}})
	if err != nil {
		t.Fatalf("authn: %v", err)
	}
	srv, err := New("127.0.0.1:0", NewService(pipe, store, logger), authn, logger)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Run(ctx) }()

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

func withKey(key string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "x-api-key", key)
}

func TestGRPCAuthUnauthenticatedAndForbidden(t *testing.T) {
	t.Parallel()
	client, _ := newAuthedGRPC(t)
	batch := &metricspb.Batch{AgentId: "a1", Metrics: []*metricspb.Metric{protoMetric("cpu", 1, time.Now())}}

	// No credentials.
	if _, err := client.Ingest(context.Background(), batch); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no creds: got %v want Unauthenticated", status.Code(err))
	}
	// Reader may not ingest.
	if _, err := client.Ingest(withKey("a-reader"), batch); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("reader ingest: got %v want PermissionDenied", status.Code(err))
	}
	// Writer may.
	if _, err := client.Ingest(withKey("a-writer"), batch); err != nil {
		t.Fatalf("writer ingest: %v", err)
	}
}

func TestGRPCTenantIsolation(t *testing.T) {
	t.Parallel()
	client, store := newAuthedGRPC(t)
	base := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

	if _, err := client.Ingest(withKey("a-writer"), &metricspb.Batch{
		AgentId: "a1", Metrics: []*metricspb.Metric{protoMetric("cpu", 5, base)},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	waitPoints(t, store, 1)

	count := func(key string) int {
		stream, err := client.Query(withKey(key), &metricspb.QueryRequest{Name: "cpu"})
		if err != nil {
			t.Fatalf("query %s: %v", key, err)
		}
		n := 0
		for {
			_, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("recv %s: %v", key, err)
			}
			n++
		}
		return n
	}

	if got := count("a-reader"); got != 1 {
		t.Errorf("tenant-a reader sees %d, want 1", got)
	}
	if got := count("b-reader"); got != 0 {
		t.Errorf("tenant-b reader sees %d, want 0 (isolation breach)", got)
	}
}
