// Package health serves Kubernetes-shaped liveness, readiness and startup
// probes, and keeps the three apart because conflating them is a classic way to
// turn a degradation into an outage.
//
// Liveness (/healthz) answers "is this process wedged?". A failing liveness
// probe restarts the container, so it must depend on nothing external. The
// textbook outage is a liveness probe that checks the database: the database has
// one bad minute, every replica's liveness fails at the same moment, and the
// orchestrator restarts the whole fleet at once — turning a brief dependency
// blip into a full outage while the database is trying to recover. So Live here
// consults nothing and always returns 200.
//
// Readiness (/readyz) answers "should traffic be routed here?". It may check
// dependencies. Failing it removes the pod from the Service endpoints without
// restarting it, so the pod keeps running and rejoins when its dependencies
// recover. Readiness also carries the shutdown gate (SetReady(false)), which a
// draining pod flips before it stops accepting connections so the load balancer
// stops sending work while in-flight requests finish. A draining pod therefore
// reports "not ready" while its checks still read "ok" — the operator can tell
// "stop sending me work" from "this pod is broken".
//
// Startup (/startupz) answers "is initialisation finished?". It exists so that a
// slow boot — replaying a large write-ahead log, warming a cache — does not trip
// the liveness probe and restart a process that is merely still starting. It
// must be a fact about initialisation (MarkStarted, called when boot completes),
// never a timer: time.Since(start) < 5*time.Second is a lie that reports ready
// before the WAL is actually replayed, and reports not-ready if the WAL replay
// happens to run long.
//
// The probe handlers are O(1). The kubelet polls them every few seconds; a
// handler that took the storage lock to check a dependency would turn a slow
// disk into a fleet-wide outage — and when the kubelet's own timeout fired, the
// handler's goroutine would still be holding that lock. So the checks run on a
// background goroutine (Run) on a ticker, publish an immutable snapshot through
// an atomic pointer, and the handlers only read the last snapshot. No handler
// ever calls a check or takes a lock that a check could be holding.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultInterval = 5 * time.Second
	defaultTimeout  = 2 * time.Second
)

// CheckFunc reports the health of one readiness dependency. It must honour ctx:
// a check that ignores cancellation cannot be interrupted and forces the timeout
// path in Probe, which leaks the check's goroutine until it returns on its own.
type CheckFunc func(context.Context) error

// Options configures a Checker. The zero value is valid and gets the defaults.
type Options struct {
	// Interval is the time between probe rounds. Defaults to 5s.
	Interval time.Duration
	// Timeout bounds each individual check within a round. Defaults to 2s.
	Timeout time.Duration
}

// CheckFunc results are published as this map: check name to the last error it
// returned, nil for a pass. It is built fresh each round and never mutated after
// it is stored, so a handler that loads the pointer sees one whole consistent
// round even while the next round is being assembled.
type probeResult map[string]error

// errPending marks a registered check that has not completed a probe round yet.
// An unprobed dependency counts as failing, not as absent: it is reported as
// "pending" so an operator can distinguish a dependency that is failing from one
// that simply has not been measured yet.
var errPending = errors.New("pending")

// Status is the JSON body of /readyz. It is emitted on both 200 and 503 so an
// operator can always `curl -s .../readyz | jq` and see which check is at fault.
type Status struct {
	// Status is "ok", "not ready" or "starting".
	Status string `json:"status"`
	// Checks maps each registered check to "ok", "pending" or its error text. It
	// is omitted while starting, when the checks are not yet the interesting fact.
	Checks map[string]string `json:"checks,omitempty"`
}

type namedCheck struct {
	name string
	fn   CheckFunc
}

// Checker runs readiness checks on a background goroutine and serves the three
// probe endpoints from the last published snapshot.
type Checker struct {
	logger   *slog.Logger
	interval time.Duration
	timeout  time.Duration

	// mu guards only the checks slice. It is held to append (Register) and to
	// copy the slice at the top of a round (Probe); it is never held while a
	// check runs, so a slow check cannot block a handler or a registration.
	mu     sync.Mutex
	checks []namedCheck

	// started and ready are independent facts a handler reads with a single
	// atomic load, so no handler needs a lock. started is monotonic once set.
	started atomic.Bool
	ready   atomic.Bool

	// results is the last completed round, swapped in whole. Handlers read it
	// with one atomic load and only ever read the map behind it.
	results atomic.Pointer[probeResult]
}

// New returns a Checker. A nil logger uses slog.Default(); a zero Options gets
// the default interval and timeout.
func New(logger *slog.Logger, opts Options) *Checker {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	return &Checker{
		logger:   logger,
		interval: opts.Interval,
		timeout:  opts.Timeout,
	}
}

// Register adds a readiness dependency. It is meant to be called during setup,
// before Run. Calling it after Run is not supported: a round in flight may
// overwrite the registration's seed, so the new check can be skipped until the
// round after next. It is nonetheless memory-safe — the checks slice is guarded
// by a mutex and the snapshot is an atomic pointer — so a stray late Register
// races on meaning, not on memory.
func (c *Checker) Register(name string, fn CheckFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks = append(c.checks, namedCheck{name: name, fn: fn})

	// Seed the published snapshot so a handler polled before the first round
	// reports this check as pending rather than silently leaving it out. Without
	// the seed, a pod would read "ready" in the window between registering a
	// dependency and first measuring it.
	seeded := make(probeResult, len(c.checks))
	if cur := c.results.Load(); cur != nil {
		for k, v := range *cur {
			seeded[k] = v
		}
	}
	seeded[name] = errPending
	c.results.Store(&seeded)
}

// MarkStarted records that initialisation finished. Until it is called both
// /startupz and /readyz fail. It is monotonic: once started, a Checker never
// reports "starting" again.
func (c *Checker) MarkStarted() { c.started.Store(true) }

// SetReady gates readiness independently of the checks. Shutdown calls
// SetReady(false) before it stops accepting connections, so the load balancer
// stops sending work while in-flight requests drain — and /readyz then reports
// "not ready" with every check still "ok", which is how draining is told apart
// from broken.
func (c *Checker) SetReady(ready bool) { c.ready.Store(ready) }

// Probe runs one round of every registered check and publishes the result. Run
// calls it on a ticker; a caller should also call it once before serving so that
// /readyz is meaningful on the very first poll.
//
// Each check runs concurrently with its own timeout derived from ctx.
func (c *Checker) Probe(ctx context.Context) {
	c.mu.Lock()
	checks := make([]namedCheck, len(c.checks))
	copy(checks, c.checks)
	c.mu.Unlock()

	result := make(probeResult, len(checks))
	var resultMu sync.Mutex
	var wg sync.WaitGroup
	for _, nc := range checks {
		wg.Add(1)
		go func(nc namedCheck) {
			defer wg.Done()
			err := c.runOne(ctx, nc)
			resultMu.Lock()
			result[nc.name] = err
			resultMu.Unlock()
		}(nc)
	}
	wg.Wait()

	c.results.Store(&result)
}

// runOne runs one check under its own timeout and returns its result, or a
// failure if the check exceeds the timeout.
//
// The check runs on its own goroutine that writes to a buffered(1) channel. This
// is the price of Go having no way to cancel a goroutine from outside: a check
// that ignores its context and blocks cannot be stopped, so on timeout we record
// the failure and move on, abandoning the goroutine. The channel is buffered so
// that the abandoned goroutine's eventual send does not block forever and leak
// its stack; when the check finally returns, it drops its now-ignored result
// into the buffer and exits. A check that never returns at all leaks exactly one
// goroutine per round — unavoidable without cancellation, and a reason to write
// checks that respect their context.
func (c *Checker) runOne(ctx context.Context, nc namedCheck) error {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		defer func() {
			// A panicking check would otherwise crash the whole process from a
			// background goroutine, turning a bug in one dependency's health check
			// into an outage of everything this binary serves. Convert it to a
			// failed check instead.
			if r := recover(); r != nil {
				c.logger.Error("health check panicked", "check", nc.name, "panic", r)
				done <- fmt.Errorf("panic: %v", r)
			}
		}()
		done <- nc.fn(cctx)
	}()

	select {
	case err := <-done:
		return err
	case <-cctx.Done():
		return cctx.Err()
	}
}

// Run probes on a ticker until ctx is done, then returns nil. It does not probe
// immediately: the caller is expected to have called Probe once before serving.
func (c *Checker) Run(ctx context.Context) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.Probe(ctx)
		}
	}
}

// Live reports that the process is running. It consults nothing — not the
// checks, not the readiness gate — because a failing liveness probe restarts the
// container, and no external dependency's health is a reason to restart this
// process. See the package doc for the outage this prevents.
func (c *Checker) Live(w http.ResponseWriter, _ *http.Request) {
	// A cached probe response is a probe that lies; no probe response may be
	// cached by an intermediary.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// Startup reports whether initialisation has finished. It is 503 until
// MarkStarted and 200 forever after, so that a slow boot does not trip liveness.
func (c *Checker) Startup(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !c.started.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("starting"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// Ready reports whether traffic should be routed here. It is 200 only when
// initialisation has finished, the readiness gate is open and every registered
// check's last round passed. The body is Status as JSON on both 200 and 503.
func (c *Checker) Ready(w http.ResponseWriter, _ *http.Request) {
	status := c.status()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if status.Status == "ok" {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(status)
}

// status builds the readiness view from the atomics and the last snapshot. It
// reads only published state and calls no check, which is what keeps the handler
// O(1).
func (c *Checker) status() Status {
	if !c.started.Load() {
		// Not started: the dependencies are not why we are not ready, boot is.
		return Status{Status: "starting"}
	}

	var checks map[string]string
	allOK := true
	if results := c.results.Load(); results != nil {
		checks = make(map[string]string, len(*results))
		for name, err := range *results {
			if err != nil {
				checks[name] = err.Error()
				allOK = false
			} else {
				checks[name] = "ok"
			}
		}
	}

	// Readiness gates on three independent facts: initialisation finished
	// (checked above), the gate is open, and every dependency passed. The gate is
	// kept separate from the checks so a draining pod reports "not ready" while
	// its checks still read "ok".
	if c.ready.Load() && allOK {
		return Status{Status: "ok", Checks: checks}
	}
	return Status{Status: "not ready", Checks: checks}
}

// Routes registers /healthz, /readyz and /startupz on mux.
func (c *Checker) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", c.Live)
	mux.HandleFunc("GET /readyz", c.Ready)
	mux.HandleFunc("GET /startupz", c.Startup)
}
