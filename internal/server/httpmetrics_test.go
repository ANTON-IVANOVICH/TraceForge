package server

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"metrics-system/internal/promexport"
)

// gather renders the metrics into a series -> value map keyed by the full
// rendered series (name plus labels), which is what a scraper sees.
func gather(t *testing.T, m *HTTPMetrics) map[string]float64 {
	t.Helper()
	var buf strings.Builder
	if err := promexport.Write(&buf, m.Gather()); err != nil {
		t.Fatalf("rendering metrics: %v", err)
	}

	out := make(map[string]float64)
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			t.Fatalf("unparseable line %q", line)
		}
		var v float64
		if _, err := fmt.Sscanf(line[i+1:], "%g", &v); err != nil {
			t.Fatalf("unparseable value in %q: %v", line, err)
		}
		out[line[:i]] = v
	}
	return out
}

// TestRouteLabelIsThePatternNotThePath. The route label has to be bounded, and
// the only bounded thing about a request is the pattern it matched. Labelling by
// r.URL.Path lets anyone with a keyboard mint metric series until the process is
// out of memory.
func TestRouteLabelIsThePatternNotThePath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/rules/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	m := NewHTTPMetrics()
	h := m.Middleware(mux)(mux)

	// Ten distinct paths, one pattern.
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/rules/rule-%d", i), nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	fams := gather(t, m)
	const want = `traceforge_http_requests_total{method="GET",route="GET /api/v1/rules/{id}",status="200"}`
	if got := fams[want]; got != 10 {
		t.Errorf("%s = %v, want 10", want, got)
	}
	for name := range fams {
		if strings.Contains(name, "rule-") {
			t.Errorf("a request path leaked into a label: %s", name)
		}
	}
}

// TestUnroutedRequestsShareOneSeries covers the three ways a request can arrive
// without a pattern: no such path, and the wrong method on a path that exists.
func TestUnroutedRequestsShareOneSeries(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /known", func(w http.ResponseWriter, _ *http.Request) {})

	m := NewHTTPMetrics()
	h := m.Middleware(mux)(mux)

	// A path nobody registered.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nope/"+strings.Repeat("a", 40), nil))
	// The right path, the wrong method: Go's mux answers 405 and matches no pattern.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/known", nil))
	// A method the standard library does not name at all.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("FROBNICATE", "/known", nil))

	fams := gather(t, m)

	var routes []string
	for name := range fams {
		if !strings.HasPrefix(name, "traceforge_http_requests_total{") {
			continue
		}
		routes = append(routes, name)
		if strings.Contains(name, `method="FROBNICATE"`) {
			t.Errorf("an arbitrary request method became a label: %s", name)
		}
	}
	if len(routes) != 3 {
		t.Errorf("got %d request series, want 3 (404, 405, unknown-method); series:\n%s",
			len(routes), strings.Join(routes, "\n"))
	}
	for _, name := range routes {
		if !strings.Contains(name, `route="other"`) {
			t.Errorf("unrouted request did not land in route=other: %s", name)
		}
	}
}

// TestRedirectsCarryTheirTargetPattern pins a surprise in net/http, and the
// comment in httpmetrics.go that describes it.
//
// A 404 and a 405 have no pattern, so they fold into route="other". A redirect
// does not: ServeMux answers /foo with a redirect to /foo/ and reports the
// registered pattern of the target. That is safe — a registered pattern is a
// bounded set — but it means a route's counter can show a 3xx for a path nobody
// wrote a handler for. The exact status is the mux's business and has changed
// across releases, so the test reads it rather than asserting it.
func TestRedirectsCarryTheirTargetPattern(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /foo/", func(w http.ResponseWriter, _ *http.Request) {})

	m := NewHTTPMetrics()
	h := m.Middleware(mux)(mux)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/foo", nil))
	if rec.Code < 300 || rec.Code >= 400 {
		t.Fatalf("GET /foo returned %d, want a redirect to /foo/", rec.Code)
	}

	// The status the mux chooses is its business (it has changed across releases);
	// what this test pins is the label, and that it is the registered pattern and
	// not the requested path.
	want := fmt.Sprintf(`traceforge_http_requests_total{method="GET",route="GET /foo/",status="%d"}`, rec.Code)

	fams := gather(t, m)
	if got := fams[want]; got != 1 {
		var series []string
		for name := range fams {
			if strings.HasPrefix(name, "traceforge_http_requests_total{") {
				series = append(series, name)
			}
		}
		t.Errorf("%s = %v, want 1; got series:\n%s", want, got, strings.Join(series, "\n"))
	}
	for name := range fams {
		if strings.Contains(name, `route="other"`) {
			t.Errorf("a redirect was folded into route=other (%s); the comment in httpmetrics.go says otherwise", name)
		}
	}
}

// TestHijackedConnectionsDoNotPoisonTheLatencyHistogram.
//
// A WebSocket lives as long as the browser tab. Feeding its lifetime into the
// request-latency histogram would move the p99 of the entire server to "one hour"
// and destroy the only latency signal there is. The request is still counted —
// as a 101 — but its duration is meaningless and is not observed.
func TestHijackedConnectionsDoNotPoisonTheLatencyHistogram(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	})
	mux.HandleFunc("GET /plain", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	m := NewHTTPMetrics()
	// httptest.NewServer, not a ResponseRecorder: only a real connection can be
	// hijacked.
	srv := httptest.NewServer(m.Middleware(mux)(mux))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/plain")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// A hijacked handler closes the connection without a response, so the client
	// sees an EOF. That is the point; the error is expected.
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	_, _ = conn.Read(buf) // blocks until the handler closes it
	_ = conn.Close()

	fams := gather(t, m)

	const wsCounter = `traceforge_http_requests_total{method="GET",route="GET /ws",status="101"}`
	if got := fams[wsCounter]; got != 1 {
		t.Errorf("%s = %v, want 1 (a hijacked request is still a request)", wsCounter, got)
	}

	// No latency series at all for the hijacked route.
	const wsHist = `traceforge_http_request_duration_seconds_count{method="GET",route="GET /ws"}`
	if got, ok := fams[wsHist]; ok {
		t.Errorf("%s = %v, want the series to be absent: a WebSocket's lifetime is not a request latency",
			wsHist, got)
	}

	// The ordinary request is in the histogram, so the absence above is a decision
	// and not a broken middleware.
	const plainHist = `traceforge_http_request_duration_seconds_count{method="GET",route="GET /plain"}`
	if got := fams[plainHist]; got != 1 {
		t.Errorf("%s = %v, want 1", plainHist, got)
	}
}

// TestPanicsAreCountedAsErrors. The metrics middleware sits outside Recover, so a
// request that panics is a 500 by the time it is counted. Wrapped the other way
// round, the code after next.ServeHTTP would never run for the requests you most
// want in the error rate.
func TestPanicsAreCountedAsErrors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})

	m := NewHTTPMetrics()
	h := Chain(mux, m.Middleware(mux), Recover(testLogger()))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("Recover produced %d, want 500", rec.Code)
	}

	fams := gather(t, m)
	const want = `traceforge_http_requests_total{method="GET",route="GET /boom",status="500"}`
	if got := fams[want]; got != 1 {
		t.Errorf("%s = %v, want 1; a panicking request must appear in the error rate", want, got)
	}
	if got := fams["traceforge_http_requests_in_flight"]; got != 0 {
		t.Errorf("in-flight = %v after a panic, want 0; the deferred decrement did not run", got)
	}
}

// TestInFlightReturnsToZero under concurrency, and the counters add up.
func TestInFlightReturnsToZeroUnderLoad(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /x", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	m := NewHTTPMetrics()
	h := m.Middleware(mux)(mux)

	const goroutines, each = 8, 50
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
			}
		}()
	}
	wg.Wait()

	fams := gather(t, m)
	const counter = `traceforge_http_requests_total{method="GET",route="GET /x",status="202"}`
	if got := fams[counter]; got != goroutines*each {
		t.Errorf("%s = %v, want %d", counter, got, goroutines*each)
	}
	const hist = `traceforge_http_request_duration_seconds_count{method="GET",route="GET /x"}`
	if got := fams[hist]; got != goroutines*each {
		t.Errorf("%s = %v, want %d", hist, got, goroutines*each)
	}
	if got := fams["traceforge_http_requests_in_flight"]; got != 0 {
		t.Errorf("in-flight = %v, want 0", got)
	}
}

// TestMetricsOutputIsValidExposition: the middleware's own output must survive
// Validate, or one bad label name would blank an entire scrape.
func TestMetricsOutputIsValidExposition(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /a/{id}", func(w http.ResponseWriter, _ *http.Request) {})
	m := NewHTTPMetrics()
	h := m.Middleware(mux)(mux)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/a/1", nil))

	for _, f := range m.Gather() {
		if err := f.Validate(); err != nil {
			t.Errorf("family %s does not validate: %v", f.Name, err)
		}
	}
}
