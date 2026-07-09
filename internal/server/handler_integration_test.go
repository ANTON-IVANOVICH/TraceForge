//go:build integration

package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"metrics-system/internal/auth"
	"metrics-system/internal/model"
	"metrics-system/internal/server/pipeline"
	"metrics-system/internal/server/storage"
	"metrics-system/internal/server/storage/bolt"
	"metrics-system/internal/testutil"
)

// These drive a real HTTP server (httptest) over a real bbolt backend, exercising
// the async ingest->store boundary, backpressure, auth/tenant isolation against a
// persistent store, and the server's graceful-shutdown lifecycle. testLogger,
// authReq and cpuBatch are shared with the package's other test files.

func boltStore(t *testing.T) storage.Storage {
	t.Helper()
	s, err := bolt.New(filepath.Join(t.TempDir(), "db.bolt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// pollQuery is a non-fatal GET for use inside Eventually: any transport error or
// non-200 yields no metrics so the poll simply retries.
func pollQuery(url, apiKey string) []model.Metric {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var got []model.Metric
	_ = json.NewDecoder(resp.Body).Decode(&got)
	return got
}

// TestHTTP_IngestThenQueryDurableOverBolt posts a metric, gets 202, then waits
// (the pipeline stores asynchronously) for it to appear via a real query against
// bbolt. This crosses the async boundary the synchronous handler_test.go cannot.
func TestHTTP_IngestThenQueryDurableOverBolt(t *testing.T) {
	store := boltStore(t)
	pipe := pipeline.New(store, pipeline.Config{}, testLogger())
	pipe.Start()
	t.Cleanup(pipe.Shutdown)

	h := NewHandler(pipe, store, testLogger())
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)

	resp := authReq(t, http.MethodPost, srv.URL+"/api/v1/metrics", "", cpuBatch)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ingest: got %d, want 202", resp.StatusCode)
	}

	testutil.Eventually(t, 3*time.Second, 20*time.Millisecond, func() bool {
		got := pollQuery(srv.URL+"/api/v1/query?name=cpu", "")
		return len(got) == 1 && got[0].Value == 7
	}, "metric never became queryable through bbolt")
}

// TestHTTP_BackpressureReturns503 wires a 1-slot ingest buffer to a pipeline that
// is never started (so nothing drains it): the first post fills the buffer, every
// later post must get 503 with a Retry-After.
func TestHTTP_BackpressureReturns503(t *testing.T) {
	store := boltStore(t)
	pipe := pipeline.New(store, pipeline.Config{IngestBuffer: 1}, testLogger())
	t.Cleanup(pipe.Shutdown)

	h := NewHandler(pipe, store, testLogger())
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)

	first := authReq(t, http.MethodPost, srv.URL+"/api/v1/metrics", "", cpuBatch)
	_ = first.Body.Close()
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first post: got %d, want 202", first.StatusCode)
	}
	for i := 0; i < 5; i++ {
		r := authReq(t, http.MethodPost, srv.URL+"/api/v1/metrics", "", cpuBatch)
		if r.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("post %d under backpressure: got %d, want 503", i, r.StatusCode)
		}
		if r.Header.Get("Retry-After") == "" {
			t.Errorf("post %d: 503 missing Retry-After header", i)
		}
		_ = r.Body.Close()
	}
}

func newAuthedBoltServer(t *testing.T) *httptest.Server {
	t.Helper()
	store := boltStore(t)
	logger := testLogger()
	pipe := pipeline.New(store, pipeline.Config{}, logger)
	pipe.Start()
	t.Cleanup(pipe.Shutdown)

	authn, err := auth.NewAPIKeyAuthenticator(auth.APIKeyConfig{Keys: []auth.APIKeyEntry{
		{Key: "a-writer", Subject: "wa", Tenant: "tenant-a", Roles: []string{"writer"}},
		{Key: "a-reader", Subject: "ra", Tenant: "tenant-a", Roles: []string{"reader"}},
		{Key: "b-reader", Subject: "rb", Tenant: "tenant-b", Roles: []string{"reader"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(pipe, store, logger)
	srv := httptest.NewServer(Chain(h.Routes(), Authenticate(authn, logger)))
	t.Cleanup(srv.Close)
	return srv
}

// TestHTTP_AuthAndTenantIsolationOverBolt checks the RBAC verdicts over real HTTP
// and, crucially, that tenant isolation holds against data actually segregated in
// bbolt: tenant B can never read tenant A's series.
func TestHTTP_AuthAndTenantIsolationOverBolt(t *testing.T) {
	srv := newAuthedBoltServer(t)
	metricsURL := srv.URL + "/api/v1/metrics"

	cases := []struct {
		name, apiKey string
		want         int
	}{
		{"no credentials", "", http.StatusUnauthorized},
		{"reader ingest", "a-reader", http.StatusForbidden},
		{"writer ingest", "a-writer", http.StatusAccepted},
	}
	for _, c := range cases {
		r := authReq(t, http.MethodPost, metricsURL, c.apiKey, cpuBatch)
		if r.StatusCode != c.want {
			t.Fatalf("%s: got %d, want %d", c.name, r.StatusCode, c.want)
		}
		_ = r.Body.Close()
	}

	queryURL := srv.URL + "/api/v1/query?name=cpu"
	testutil.Eventually(t, 3*time.Second, 20*time.Millisecond, func() bool {
		return len(pollQuery(queryURL, "a-reader")) == 1
	}, "tenant-a writer's metric never became queryable")

	if got := pollQuery(queryURL, "b-reader"); len(got) != 0 {
		t.Errorf("tenant-b reader saw %d of tenant-a's metrics (isolation breach)", len(got))
	}
}

// TestHTTP_GracefulShutdownDrainsInflight starts a server via New+Run, fires a
// slow request, cancels the context mid-request, and asserts the in-flight
// request still completes and Run returns nil.
func TestHTTP_GracefulShutdownDrainsInflight(t *testing.T) {
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond) // still running when shutdown is requested
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("done"))
	})

	srv, err := New("127.0.0.1:0", mux, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(ctx) }()

	type result struct {
		status int
		body   string
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + srv.Addr() + "/slow")
		if err != nil {
			resCh <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		resCh <- result{status: resp.StatusCode, body: string(b)}
	}()

	<-started // the handler is running
	cancel()  // request shutdown with the request still in flight

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("in-flight request failed instead of draining: %v", res.err)
		}
		if res.status != http.StatusOK || res.body != "done" {
			t.Fatalf("in-flight request got %d/%q, want 200/done", res.status, res.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not complete during graceful shutdown")
	}

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v, want nil after graceful shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}
