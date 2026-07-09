package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"metrics-system/internal/server/pipeline"
	"metrics-system/internal/server/storage"
)

// /debug/pprof/cmdline prints the process's argv, and the server takes
// -jwt-hs256-secret on argv. If pprof is ever wired back onto the API mux, this
// test fails and says why.
func TestAPIMuxDoesNotServePprof(t *testing.T) {
	h := NewHandler(pipeline.New(storage.NewMemoryStorage(), pipeline.Config{}, testLogger()),
		storage.NewMemoryStorage(), testLogger())
	routes := h.Routes()

	for _, path := range []string{
		"/debug/pprof/",
		"/debug/pprof/cmdline",
		"/debug/pprof/heap",
		"/debug/pprof/profile",
		"/debug/pprof/trace",
	} {
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s on the API mux: want 404, got %d — pprof must live on its own listener", path, rec.Code)
		}
	}
}

func TestProfilingServerServesPprofAndNothingElse(t *testing.T) {
	srv, err := NewProfilingServer("127.0.0.1:0", testLogger())
	if err != nil {
		t.Fatalf("NewProfilingServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.listener.Close() })

	rec := httptest.NewRecorder()
	srv.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /debug/pprof/: want 200, got %d", rec.Code)
	}

	// The profiling listener is not an API. Anything that is not pprof is a 404,
	// so a misconfigured reverse proxy cannot accidentally expose ingest on it.
	for _, path := range []string{"/api/v1/query", "/healthz", "/debug/stats", "/"} {
		rec := httptest.NewRecorder()
		srv.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s on the pprof listener: want 404, got %d", path, rec.Code)
		}
	}
}

func TestServerAddrReportsTheKernelAssignedPort(t *testing.T) {
	srv, err := New("127.0.0.1:0", http.NotFoundHandler(), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.listener.Close() })

	addr := srv.Addr()
	if addr == "127.0.0.1:0" || addr == "" {
		t.Fatalf("Addr must report the bound port, got %q", addr)
	}
}

func TestNewFailsFastOnAnAddressAlreadyInUse(t *testing.T) {
	first, err := New("127.0.0.1:0", http.NotFoundHandler(), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = first.listener.Close() })

	if _, err := New(first.Addr(), http.NotFoundHandler(), testLogger()); err == nil {
		t.Fatal("binding an address already in use must fail at New, not silently inside Run")
	}
}

func TestIsLoopback(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:6060", true},
		{"[::1]:6060", true},
		{"localhost:6060", true},
		{"0.0.0.0:6060", false},
		{":6060", false},     // every interface — the case worth warning about
		{"[::]:6060", false}, // ditto, in v6 clothing
		{"10.0.0.5:6060", false},
		{"metrics.example.com:6060", false},
		{"not-an-address", false},
	}
	for _, tt := range tests {
		if got := isLoopback(tt.addr); got != tt.want {
			t.Errorf("isLoopback(%q) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}
