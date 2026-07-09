package testutil

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A leaked goroutine is the most common way a Go service dies slowly: goroutines
// are cheap to start and easy to forget, and a handler that spawns one per
// request without a way out will sit at fifty thousand of them an hour later.
// Nothing fails, nothing logs, memory climbs, and the process is eventually
// killed.
//
// The detector below is deliberately small. It snapshots the set of goroutine
// IDs alive when it is installed, and at test cleanup it reports any goroutine
// that appeared during the test and is still running. Comparing IDs rather than
// counts is what makes it precise: a test that starts one goroutine and leaks a
// different one shows a stable count and a real leak.

// NoLeaks installs a goroutine-leak check that runs when the test finishes.
// Call it first, before starting anything:
//
//	func TestHub(t *testing.T) {
//	    defer testutil.NoLeaks(t)()
//	    ...
//	}
//
// or simply `testutil.NoLeaks(t)`, which registers a t.Cleanup. The returned
// function lets a caller check earlier, at a point of its choosing.
//
// It is intentionally not compatible with t.Parallel(): parallel siblings start
// and stop goroutines inside each other's windows, and every check would be a
// coin flip.
func NoLeaks(tb testing.TB) func() {
	tb.Helper()
	before := goroutineIDs()

	var checked bool
	check := func() {
		if checked {
			return
		}
		checked = true
		tb.Helper()
		if tb.Failed() {
			// A failing test often abandons goroutines on purpose (t.Fatal from a
			// helper, a server never shut down). Reporting a leak on top of the
			// real failure buries it.
			return
		}
		if leaked := findLeaks(before, time.Second); len(leaked) > 0 {
			tb.Errorf("leaked %d goroutine(s) after the test returned:\n\n%s",
				len(leaked), strings.Join(leaked, "\n\n"))
		}
	}
	tb.Cleanup(check)
	return check
}

// findLeaks retries until timeout before declaring a leak. A goroutine told to
// stop needs a scheduling quantum to actually stop, and the race detector makes
// that quantum longer; without the retry this is the flakiest check in the
// suite.
func findLeaks(before map[uint64]bool, timeout time.Duration) []string {
	deadline := time.Now().Add(timeout)
	var leaked []string
	for {
		leaked = nil
		for _, g := range goroutines() {
			if before[g.id] || ignored(g) {
				continue
			}
			leaked = append(leaked, g.stack)
		}
		if len(leaked) == 0 || time.Now().After(deadline) {
			return leaked
		}
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
}

type goroutine struct {
	id    uint64
	stack string
}

// goroutineIDs snapshots the IDs of every goroutine alive right now.
func goroutineIDs() map[uint64]bool {
	gs := goroutines()
	ids := make(map[uint64]bool, len(gs))
	for _, g := range gs {
		ids[g.id] = true
	}
	return ids
}

// goroutines parses runtime.Stack(all=true). There is no supported API for
// enumerating goroutines — the text dump is the API, and it has been stable for
// a decade.
func goroutines() []goroutine {
	buf := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf)) // truncated: the dump needs more room
	}

	var out []goroutine
	for _, block := range strings.Split(string(buf), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		g, ok := parseGoroutine(block)
		if ok {
			out = append(out, g)
		}
	}
	return out
}

// parseGoroutine reads the header line "goroutine 42 [chan receive, 3 minutes]:".
func parseGoroutine(block string) (goroutine, bool) {
	header, _, ok := strings.Cut(block, "\n")
	if !ok {
		return goroutine{}, false
	}
	rest, ok := strings.CutPrefix(header, "goroutine ")
	if !ok {
		return goroutine{}, false
	}
	idStr, _, ok := strings.Cut(rest, " [")
	if !ok {
		return goroutine{}, false
	}
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return goroutine{}, false
	}
	return goroutine{id: id, stack: block}, true
}

// ignoredFrames are goroutines that the runtime, the testing package or the
// standard library start on their own schedule. They may appear after the
// snapshot was taken and are not the test's to clean up.
//
// Each entry is a substring of a stack frame, matched against the whole dump of
// one goroutine. The list is short on purpose: a long ignore list is how a leak
// detector stops detecting leaks.
var ignoredFrames = []string{
	"testing.(*T).Run",
	"testing.(*T).Parallel",
	"testing.tRunner",
	"testing.runFuzzTests",
	"testing.runFuzzing",
	"runtime.gcBgMarkWorker",
	"runtime.bgsweep",
	"runtime.bgscavenge",
	"runtime.forcegchelper",
	"runtime.runfinq",
	"runtime/trace.Start",
	"os/signal.signal_recv",
	"os/signal.loop",
	// net/http keeps idle connections (and their read/write loops) alive after a
	// response for reuse. httptest.Server.Close does not wait for the client's
	// side of them, and CloseIdleConnections is asynchronous.
	"net/http.(*persistConn).readLoop",
	"net/http.(*persistConn).writeLoop",
	"net/http.(*Transport).dialConnFor",
	// The profiler's own writer, started by pprof.StartCPUProfile.
	"runtime/pprof.profileWriter",
}

func ignored(g goroutine) bool {
	for _, frame := range ignoredFrames {
		if strings.Contains(g.stack, frame) {
			return true
		}
	}
	return false
}
