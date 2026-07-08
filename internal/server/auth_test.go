package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"metrics-system/internal/auth"
	"metrics-system/internal/model"
	"metrics-system/internal/server/pipeline"
	"metrics-system/internal/server/storage"
)

func newAuthedServer(t *testing.T) (*httptest.Server, storage.Storage) {
	t.Helper()
	store := storage.NewMemoryStorage()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
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
	h := NewHandler(pipe, store, logger)
	srv := httptest.NewServer(Chain(h.Routes(), Authenticate(authn, logger)))
	t.Cleanup(func() {
		srv.Close()
		pipe.Shutdown()
		_ = store.Close()
	})
	return srv, store
}

func authReq(t *testing.T, method, url, apiKey, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

const cpuBatch = `{"agent_id":"a1","metrics":[{"name":"cpu","type":"gauge","value":7,"timestamp":"2026-07-09T12:00:00Z"}]}`

func TestHTTPAuthRejectsAndAuthorizes(t *testing.T) {
	t.Parallel()
	srv, _ := newAuthedServer(t)

	// No credentials -> 401.
	if r := authReq(t, http.MethodPost, srv.URL+"/api/v1/metrics", "", cpuBatch); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no creds: got %d want 401", r.StatusCode)
	}
	// Reader trying to ingest -> 403.
	if r := authReq(t, http.MethodPost, srv.URL+"/api/v1/metrics", "a-reader", cpuBatch); r.StatusCode != http.StatusForbidden {
		t.Fatalf("reader ingest: got %d want 403", r.StatusCode)
	}
	// Writer ingest -> 202.
	if r := authReq(t, http.MethodPost, srv.URL+"/api/v1/metrics", "a-writer", cpuBatch); r.StatusCode != http.StatusAccepted {
		t.Fatalf("writer ingest: got %d want 202", r.StatusCode)
	}
	// healthz is public.
	if r := authReq(t, http.MethodGet, srv.URL+"/healthz", "", ""); r.StatusCode != http.StatusOK {
		t.Fatalf("healthz: got %d want 200", r.StatusCode)
	}
}

func queryCount(t *testing.T, srv *httptest.Server, apiKey string) int {
	t.Helper()
	r := authReq(t, http.MethodGet, srv.URL+"/api/v1/query?name=cpu", apiKey, "")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("query %s: got %d", apiKey, r.StatusCode)
	}
	var got []model.Metric
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_ = r.Body.Close()
	return len(got)
}

func TestHTTPTenantIsolation(t *testing.T) {
	t.Parallel()
	srv, store := newAuthedServer(t)

	// Tenant A writes one metric.
	if r := authReq(t, http.MethodPost, srv.URL+"/api/v1/metrics", "a-writer", cpuBatch); r.StatusCode != http.StatusAccepted {
		t.Fatalf("ingest: %d", r.StatusCode)
	}
	// Wait for the async pipeline to store it.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && store.Stats().Points == 0 {
		time.Sleep(20 * time.Millisecond)
	}

	if n := queryCount(t, srv, "a-reader"); n != 1 {
		t.Errorf("tenant-a reader sees %d metrics, want 1", n)
	}
	if n := queryCount(t, srv, "b-reader"); n != 0 {
		t.Errorf("tenant-b reader sees %d metrics, want 0 (isolation breach)", n)
	}
}
