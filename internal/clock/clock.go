// Package clock abstracts the passage of time so that schedulers, tickers and
// timeouts can be driven deterministically from tests. Production code uses
// Real (a thin wrapper over the time package); tests use Fake, whose Advance
// method moves time forward and fires every due ticker and timer in order.
//
// Alerting is unusually time-dependent ("fire only if the condition holds for
// 5m", "wait 30s before the first notification", "retry with backoff"), so a
// test that relies on real sleeps is both slow and flaky. Injecting a Clock is
// the standard remedy.
package clock

import "time"

// Clock is the subset of the time package the alerting stack needs.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	NewTicker(d time.Duration) Ticker
	NewTimer(d time.Duration) Timer
}

// Ticker mirrors *time.Ticker, exposing its channel through a method so that a
// fake implementation can satisfy the same interface.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Timer mirrors *time.Timer.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// Real is the production Clock, backed by the time package.
type Real struct{}

// New returns the production clock.
func New() Clock { return Real{} }

func (Real) Now() time.Time                         { return time.Now() }
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (Real) NewTicker(d time.Duration) Ticker       { return realTicker{time.NewTicker(d)} }
func (Real) NewTimer(d time.Duration) Timer         { return realTimer{time.NewTimer(d)} }

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time        { return r.t.C }
func (r realTimer) Stop() bool                 { return r.t.Stop() }
func (r realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }
