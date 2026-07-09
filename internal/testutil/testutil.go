// Package testutil holds the shared testing infrastructure: builders for domain
// objects, assertions that report at the caller's line, an Eventually helper for
// asynchronous conditions, a goroutine-leak detector and golden-file support.
//
// It is imported only from _test.go files. Nothing in the production binaries
// depends on it, which is why it may register the -update flag and reach for
// runtime introspection.
//
// Every helper here calls t.Helper() so a failure points at the test that
// triggered it rather than at the helper's own body — without that, a shared
// assertion reports the same file:line for every failing test in the suite.
package testutil

import (
	"testing"
	"time"
)

// Eventually polls cond every tick until it returns true or timeout elapses,
// then fails with msg. It is the correct tool for anything asynchronous: a
// pipeline that stores a metric a few milliseconds from now, a server that
// finishes binding, an alert that transitions to firing.
//
// It replaces the time.Sleep(2 * time.Second) reflex. Sleep is both slower (it
// always waits the worst case) and weaker (it passes if the condition held only
// at the instant you happened to look).
//
// cond must be safe to call concurrently with the code under test.
func Eventually(tb testing.TB, timeout, tick time.Duration, cond func() bool, msg string, args ...any) {
	tb.Helper()

	if cond() { // fast path: already true, do not pay a tick
		return
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	for {
		select {
		case <-timer.C:
			tb.Helper()
			tb.Fatalf("condition never held within %s: "+msg, append([]any{timeout}, args...)...)
		case <-ticker.C:
			if cond() {
				return
			}
		}
	}
}

// Never asserts that cond stays false for the whole window. It is Eventually's
// negation and is what you want for "the alert must NOT fire", where an
// Eventually that times out would pass for the wrong reason.
func Never(tb testing.TB, window, tick time.Duration, cond func() bool, msg string, args ...any) {
	tb.Helper()

	deadline := time.NewTimer(window)
	defer deadline.Stop()
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	for {
		select {
		case <-deadline.C:
			return
		case <-ticker.C:
			if cond() {
				tb.Helper()
				tb.Fatalf("condition held but should never have: "+msg, args...)
			}
		}
	}
}
