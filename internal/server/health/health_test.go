package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"metrics-system/internal/server/health"
)

// newChecker builds a Checker with a short timeout so the blocking-check test
// finishes in tens of milliseconds, and a discard logger so a panicking-check
// test does not spam the suite output.
func newChecker() *health.Checker {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return health.New(logger, health.Options{Interval: time.Millisecond, Timeout: 20 * time.Millisecond})
}

// hit drives one handler and returns the recorded response. It takes no *testing.T
// so it is safe to call from the goroutines in the race test.
func hit(h http.HandlerFunc) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func decodeStatus(t *testing.T, rec *httptest.ResponseRecorder) health.Status {
	t.Helper()
	var s health.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("body is not valid Status JSON: %v (body=%q)", err, rec.Body.String())
	}
	return s
}

// Live must never depend on the checks or the readiness gate: a failing
// dependency is not a reason to restart the container.
func TestLiveIsAlwaysOK(t *testing.T) {
	c := newChecker()
	c.Register("db", func(context.Context) error { return errors.New("down") })
	c.SetReady(false)
	// Not started, gate closed, the one check fails: every readiness input is red.
	c.Probe(context.Background())

	rec := hit(c.Live)
	if rec.Code != http.StatusOK {
		t.Fatalf("Live status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Fatalf("Live body = %q, want %q", got, "ok")
	}
}

// Startup is a fact about initialisation and monotonic: it flips to 200 once and
// never regresses, in particular not when the pod later starts draining.
func TestStartupIsMonotonic(t *testing.T) {
	c := newChecker()

	rec := hit(c.Startup)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Startup before MarkStarted = %d, want 503", rec.Code)
	}
	if got := rec.Body.String(); got != "starting" {
		t.Fatalf("Startup body before MarkStarted = %q, want %q", got, "starting")
	}

	c.MarkStarted()
	if rec := hit(c.Startup); rec.Code != http.StatusOK {
		t.Fatalf("Startup after MarkStarted = %d, want 200", rec.Code)
	}

	// Draining flips readiness, never startup: a pod that has booted has booted.
	c.SetReady(false)
	if rec := hit(c.Startup); rec.Code != http.StatusOK {
		t.Fatalf("Startup after SetReady(false) = %d, want 200 (must not regress)", rec.Code)
	}
}

// Ready gates on started AND the readiness gate AND every check's last round,
// and the body reports each check throughout so an operator can see why.
func TestReadyStateMachine(t *testing.T) {
	c := newChecker()
	var failing atomic.Bool
	c.Register("db", func(context.Context) error {
		if failing.Load() {
			return errors.New("connection refused")
		}
		return nil
	})

	// 1. Not started -> 503 "starting".
	rec := hit(c.Ready)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Ready before MarkStarted = %d, want 503", rec.Code)
	}
	if s := decodeStatus(t, rec); s.Status != "starting" {
		t.Fatalf("Ready before MarkStarted status = %q, want %q", s.Status, "starting")
	}

	// 2. Started and gate open, but the check has never been probed -> pending.
	c.MarkStarted()
	c.SetReady(true)
	rec = hit(c.Ready)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Ready with an unprobed check = %d, want 503", rec.Code)
	}
	if s := decodeStatus(t, rec); s.Checks["db"] != "pending" {
		t.Fatalf("unprobed check reported %q, want %q", s.Checks["db"], "pending")
	}

	// 3. A successful round with the gate open -> 200 "ok".
	c.Probe(context.Background())
	rec = hit(c.Ready)
	if rec.Code != http.StatusOK {
		t.Fatalf("Ready after a passing round = %d, want 200", rec.Code)
	}
	if s := decodeStatus(t, rec); s.Status != "ok" || s.Checks["db"] != "ok" {
		t.Fatalf("Ready after a passing round = %+v, want status ok and db ok", s)
	}

	// 4. The dependency starts failing -> 503, and the body names it with its text.
	failing.Store(true)
	c.Probe(context.Background())
	rec = hit(c.Ready)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Ready with a failing check = %d, want 503", rec.Code)
	}
	if s := decodeStatus(t, rec); s.Checks["db"] != "connection refused" {
		t.Fatalf("failing check reported %q, want the error text %q", s.Checks["db"], "connection refused")
	}

	// 5. The check recovers, then the pod starts draining. 503 again, but this
	// time every check reads "ok": the operator can tell draining from broken.
	failing.Store(false)
	c.Probe(context.Background())
	c.SetReady(false)
	rec = hit(c.Ready)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Ready while draining = %d, want 503", rec.Code)
	}
	s := decodeStatus(t, rec)
	if s.Status != "not ready" {
		t.Fatalf("draining status = %q, want %q", s.Status, "not ready")
	}
	if s.Checks["db"] != "ok" {
		t.Fatalf("draining pod reported db=%q, want ok so draining is distinguishable from broken", s.Checks["db"])
	}
}

// A check that ignores its context and blocks must not wedge the round: Probe
// returns within roughly the timeout, the check is reported as failing, and a
// second round is not held up by the first round's abandoned goroutine.
func TestProbeDoesNotBlockOnAHangingCheck(t *testing.T) {
	c := newChecker()
	release := make(chan struct{})
	// Unblock the abandoned goroutine at the end so the test process leaks nothing.
	t.Cleanup(func() { close(release) })
	c.Register("stuck", func(context.Context) error {
		<-release // deliberately ignores ctx: models a check that hangs
		return nil
	})
	c.MarkStarted()
	c.SetReady(true)

	assertReturnsPromptly := func(label string) {
		done := make(chan struct{})
		go func() {
			c.Probe(context.Background())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not return; a hanging check wedged the round", label)
		}
	}

	assertReturnsPromptly("first Probe")

	rec := hit(c.Ready)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Ready with a hanging check = %d, want 503", rec.Code)
	}
	if s := decodeStatus(t, rec); !strings.Contains(s.Checks["stuck"], "deadline exceeded") {
		t.Fatalf("hanging check reported %q, want a deadline-exceeded failure", s.Checks["stuck"])
	}

	// The first round's goroutine is still stuck; the second round must not care.
	assertReturnsPromptly("second Probe")
}

// The handlers must be O(1): they read the last snapshot and never call a check.
func TestHandlersNeverInvokeChecks(t *testing.T) {
	c := newChecker()
	var calls atomic.Int64
	c.Register("db", func(context.Context) error {
		calls.Add(1)
		return nil
	})
	c.MarkStarted()
	c.SetReady(true)

	for i := 0; i < 100; i++ {
		hit(c.Ready)
		hit(c.Live)
		hit(c.Startup)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("handlers invoked the check %d time(s); only Probe may run checks", n)
	}
}

// Run must actually probe on its ticker and return nil when ctx is cancelled.
// It also exercises the nil-logger and default-timeout paths of New.
func TestRunProbesOnTickerAndStops(t *testing.T) {
	c := health.New(nil, health.Options{Interval: time.Millisecond, Timeout: time.Second})
	probed := make(chan struct{}, 1)
	c.Register("db", func(context.Context) error {
		select {
		case probed <- struct{}{}:
		default:
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- c.Run(ctx) }()

	select {
	case <-probed:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not probe on its ticker")
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
	}
}

// A panicking check must be contained: it becomes a failed check, not a crash of
// the background goroutine (which would take the whole process down).
func TestPanickingCheckIsContained(t *testing.T) {
	c := newChecker()
	c.Register("boom", func(context.Context) error {
		panic("kaboom")
	})
	c.MarkStarted()
	c.SetReady(true)

	c.Probe(context.Background())

	rec := hit(c.Ready)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Ready after a panicking check = %d, want 503", rec.Code)
	}
	if s := decodeStatus(t, rec); !strings.Contains(s.Checks["boom"], "panic") {
		t.Fatalf("panicking check reported %q, want a panic failure", s.Checks["boom"])
	}
}

// Probe responses must never be cached: an intermediary caching a /readyz 200
// while the pod drains keeps the load balancer sending work to a pod that has
// already said "stop". So every handler sets Cache-Control: no-store, and Ready
// additionally declares application/json so `curl -s .../readyz | jq` is safe on
// both the 200 and the 503 body. Assert the headers on all three handlers and in
// both status states.
func TestHandlersSetNoStoreAndContentType(t *testing.T) {
	noStore := func(rec *httptest.ResponseRecorder, label string) {
		t.Helper()
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s Cache-Control = %q, want %q", label, got, "no-store")
		}
	}
	jsonType := func(rec *httptest.ResponseRecorder, label string) {
		t.Helper()
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Fatalf("%s Content-Type = %q, want %q", label, got, "application/json")
		}
	}

	c := newChecker()
	c.Register("db", func(context.Context) error { return nil })

	// Live consults nothing and is always 200.
	noStore(hit(c.Live), "Live 200")

	// Before MarkStarted the check-serving handlers are 503.
	startRec := hit(c.Startup)
	if startRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Startup before MarkStarted = %d, want 503", startRec.Code)
	}
	noStore(startRec, "Startup 503")

	readyRec := hit(c.Ready)
	if readyRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Ready before MarkStarted = %d, want 503", readyRec.Code)
	}
	noStore(readyRec, "Ready 503")
	jsonType(readyRec, "Ready 503")

	// After MarkStarted, a passing round and an open gate they flip to 200.
	c.MarkStarted()
	c.SetReady(true)
	c.Probe(context.Background())

	startRec = hit(c.Startup)
	if startRec.Code != http.StatusOK {
		t.Fatalf("Startup after MarkStarted = %d, want 200", startRec.Code)
	}
	noStore(startRec, "Startup 200")

	readyRec = hit(c.Ready)
	if readyRec.Code != http.StatusOK {
		t.Fatalf("Ready after a passing round = %d, want 200", readyRec.Code)
	}
	noStore(readyRec, "Ready 200")
	jsonType(readyRec, "Ready 200")
}

// New must apply the documented defaults for a zero or negative Interval and
// Timeout. The defaults are unexported and there is no accessor — adding one
// only for the test would export internals for no operational reason — so the
// guards are pinned behaviourally: a zero Interval would make Run's
// time.NewTicker(interval) panic, and a zero Timeout would make each check's
// context.WithTimeout(ctx, 0) expire before the check runs, so an
// instantly-succeeding check would be reported deadline-exceeded.
func TestNewAppliesDefaultsForNonPositiveOptions(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct {
		name string
		opts health.Options
	}{
		{"zero", health.Options{}},
		{"negative", health.Options{Interval: -1, Timeout: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Interval guard: Run must build its ticker without panicking and return
			// nil once ctx is done. Run it off the test goroutine so a NewTicker(0)
			// panic or a hang is reported rather than crashing or wedging the suite.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			result := make(chan error, 1)
			panicked := make(chan any, 1)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						panicked <- r
					}
				}()
				result <- health.New(logger, tc.opts).Run(ctx)
			}()
			select {
			case err := <-result:
				if err != nil {
					t.Fatalf("Run(cancelled) with %+v = %v, want nil", tc.opts, err)
				}
			case r := <-panicked:
				t.Fatalf("Run with %+v panicked (%v); the Interval default was not applied", tc.opts, r)
			case <-time.After(2 * time.Second):
				t.Fatalf("Run with %+v did not return; the Interval default was not applied", tc.opts)
			}

			// Timeout guard: an instantly-succeeding check must record success, not a
			// deadline-exceeded failure from a zero per-check timeout.
			c := health.New(logger, tc.opts)
			c.Register("db", func(context.Context) error { return nil })
			c.MarkStarted()
			c.SetReady(true)
			c.Probe(context.Background())

			rec := hit(c.Ready)
			s := decodeStatus(t, rec)
			if s.Checks["db"] != "ok" {
				t.Fatalf("with %+v an instant check reported %q, want ok; the Timeout default was not applied", tc.opts, s.Checks["db"])
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("with %+v Ready = %d, want 200", tc.opts, rec.Code)
			}
		})
	}
}

// Probe from one goroutine while every handler and SetReady are hammered from
// several. The point of this test is the race detector: run it with -race.
func TestConcurrentProbesAndHandlers(t *testing.T) {
	c := newChecker()
	var n atomic.Int64
	c.Register("db", func(context.Context) error {
		if n.Add(1)%2 == 0 {
			return errors.New("flaky")
		}
		return nil
	})
	c.MarkStarted()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			c.Probe(context.Background())
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				hit(c.Ready)
				hit(c.Live)
				hit(c.Startup)
				c.SetReady(i%2 == 0)
			}
		}()
	}

	wg.Wait()
}
