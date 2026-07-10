package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"metrics-system/internal/promexport"
	"metrics-system/internal/server/health"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// stubGatherer emits one family so /metrics has something to render.
type stubGatherer struct{}

func (stubGatherer) Gather() []promexport.Family {
	return []promexport.Family{{
		Name:    "stub_total",
		Help:    "A stub.",
		Type:    promexport.TypeCounter,
		Samples: []promexport.Sample{{Value: 7}},
	}}
}

func newChecker(t *testing.T, ready bool) *health.Checker {
	t.Helper()
	c := health.New(testLogger(), health.Options{Interval: time.Hour, Timeout: time.Second})
	c.MarkStarted()
	c.SetReady(ready)
	c.Probe(context.Background())
	return c
}

// TestHandlerServesTheAdminSurface. The kubelet and Prometheus both talk to this
// mux, and neither of them reads a changelog when a path moves.
func TestHandlerServesTheAdminSurface(t *testing.T) {
	h := Handler(Config{
		Health:    newChecker(t, true),
		Gatherers: []promexport.Gatherer{stubGatherer{}},
	}, testLogger())

	cases := []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{"/healthz", http.StatusOK, "ok"},
		{"/readyz", http.StatusOK, `"status":"ok"`},
		{"/startupz", http.StatusOK, "ok"},
		{"/metrics", http.StatusOK, "stub_total 7"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != c.wantStatus {
			t.Errorf("GET %s = %d, want %d", c.path, rec.Code, c.wantStatus)
		}
		if !strings.Contains(rec.Body.String(), c.wantBody) {
			t.Errorf("GET %s body = %q, want it to contain %q", c.path, rec.Body.String(), c.wantBody)
		}
	}

	// The exposition format has a version parameter, and it is not decoration: it
	// is what tells the scraper which escaping rules apply.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("/metrics Content-Type = %q", ct)
	}
}

// TestHandlerOmitsWhatItWasNotGiven: a nil Checker must not register probe routes
// that would answer 200 with no checks behind them, and no gatherers must mean no
// /metrics rather than an empty one.
func TestHandlerOmitsWhatItWasNotGiven(t *testing.T) {
	h := Handler(Config{}, testLogger())
	for _, path := range []string{"/healthz", "/readyz", "/startupz", "/metrics"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d with an empty Config, want 404", path, rec.Code)
		}
	}
}

// TestHandlerDoesNotServeTheAPIOrPprof. Three listeners exist because they have
// three threat models. If the admin mux ever grew a pprof route, /debug/pprof/cmdline
// would print argv — and argv is where -jwt-hs256-secret lives.
func TestHandlerDoesNotServeTheAPIOrPprof(t *testing.T) {
	h := Handler(Config{Health: newChecker(t, true), Gatherers: []promexport.Gatherer{stubGatherer{}}}, testLogger())
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline", "/api/v1/query", "/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("the telemetry mux answered %s with %d; it must serve nothing but the probes and the scrape",
				path, rec.Code)
		}
	}
}

// TestNewRefusesAnEmptyAddress: "" means "do not serve", and a Server that bound
// a random port instead would leave an unadvertised admin surface open.
func TestNewRefusesAnEmptyAddress(t *testing.T) {
	if _, err := New(Config{}, testLogger()); err == nil {
		t.Error("New with no address returned a server")
	}
}

// TestServerBindsAndStops. Binding in New rather than Run is what lets the e2e
// suite read the kernel-assigned port back out of Addr.
func TestServerBindsAndStops(t *testing.T) {
	srv, err := New(Config{Addr: "127.0.0.1:0", Health: newChecker(t, true)}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(srv.Addr(), "127.0.0.1:") || strings.HasSuffix(srv.Addr(), ":0") {
		t.Fatalf("Addr() = %q, want the port the kernel assigned", srv.Addr())
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	resp, err := http.Get("http://" + srv.Addr() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// TestSelfCheck is the flag that makes HEALTHCHECK possible in an image with no
// shell. It has to succeed against a ready server, fail against an unready one
// while saying why, and fail against nothing at all.
func TestSelfCheck(t *testing.T) {
	t.Run("no address", func(t *testing.T) {
		if err := SelfCheck(context.Background(), ""); err == nil {
			t.Error("SelfCheck with no address succeeded")
		}
	})

	t.Run("nothing listening", func(t *testing.T) {
		err := SelfCheck(context.Background(), "127.0.0.1:1")
		if err == nil {
			t.Fatal("SelfCheck succeeded against a closed port")
		}
	})

	t.Run("ready", func(t *testing.T) {
		srv, err := New(Config{Addr: "127.0.0.1:0", Health: newChecker(t, true)}, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = srv.Run(ctx) }()

		if err := SelfCheck(context.Background(), srv.Addr()); err != nil {
			t.Errorf("SelfCheck against a ready server: %v", err)
		}
	})

	t.Run("not ready names the reason", func(t *testing.T) {
		checker := newChecker(t, true)
		checker.Register("storage", func(context.Context) error { return errDisk })
		checker.Probe(context.Background())

		srv, err := New(Config{Addr: "127.0.0.1:0", Health: checker}, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = srv.Run(ctx) }()

		err = SelfCheck(context.Background(), srv.Addr())
		if err == nil {
			t.Fatal("SelfCheck succeeded against a server whose storage check is failing")
		}
		// The body names the failing check. That is the difference between
		// "unhealthy" in `docker ps` and knowing which dependency is down.
		if !strings.Contains(err.Error(), "no space left on device") {
			t.Errorf("SelfCheck error = %q, want it to quote the failing check", err)
		}
	})
}

// errDisk stands in for the failure a readiness check actually reports.
var errDisk = errors.New("no space left on device")

// TestSelfCheckDialsLoopbackForAWildcardAddress.
//
// ":9091" and "0.0.0.0:9091" mean "every interface" to a listener and nothing at
// all to a dialer. The probe runs inside the container, so loopback is both
// correct and the only address guaranteed to reach it.
func TestSelfCheckDialsLoopbackForAWildcardAddress(t *testing.T) {
	srv, err := New(Config{Addr: "127.0.0.1:0", Health: newChecker(t, true)}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()

	_, port, err := splitPort(srv.Addr())
	if err != nil {
		t.Fatal(err)
	}

	// The three spellings an operator would put in -telemetry-addr.
	for _, addr := range []string{":" + port, "0.0.0.0:" + port} {
		if err := SelfCheck(context.Background(), addr); err != nil {
			t.Errorf("SelfCheck(%q): %v — a wildcard listen address must resolve to loopback", addr, err)
		}
	}
}

func splitPort(addr string) (string, string, error) { return net.SplitHostPort(addr) }

// TestRuntimeGathererIsValidAndPlausible. The runtime families are the ones a
// dashboard reads when nothing else works, so they must at least render.
func TestRuntimeGathererIsValidAndPlausible(t *testing.T) {
	families := RuntimeGatherer().Gather()
	if len(families) < 4 {
		t.Fatalf("the runtime gatherer produced %d families", len(families))
	}

	byName := make(map[string]promexport.Family, len(families))
	for _, f := range families {
		if err := f.Validate(); err != nil {
			t.Errorf("family %s does not validate: %v", f.Name, err)
		}
		byName[f.Name] = f
	}

	for _, want := range []string{
		"traceforge_go_goroutines",
		"traceforge_go_maxprocs",
		"traceforge_go_memory_limit_bytes",
		"traceforge_go_gc_cycles_total",
	} {
		if _, ok := byName[want]; !ok {
			t.Errorf("%s is missing", want)
		}
	}

	// A gatherer that returned zeros would render, validate, and tell you nothing.
	if v := byName["traceforge_go_goroutines"].Samples[0].Value; v < 1 {
		t.Errorf("goroutines = %v; this test alone is one", v)
	}
	if v := byName["traceforge_go_maxprocs"].Samples[0].Value; v < 1 {
		t.Errorf("GOMAXPROCS = %v, want at least 1", v)
	}

	// The runtime metrics must NOT borrow client_golang's names, because a
	// community dashboard bound to `go_goroutines` would silently read ours.
	for name := range byName {
		if strings.HasPrefix(name, "go_") {
			t.Errorf("family %q impersonates a client_golang metric name", name)
		}
	}
}
