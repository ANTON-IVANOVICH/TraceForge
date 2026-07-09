package clock

import (
	"sync"
	"time"
)

// Fake is a Clock whose time only moves when Advance is called. Firing is
// chronological: advancing past several deadlines fires them in the order they
// would have occurred, with Now() set to each deadline as it fires, so code
// that reads Now() from a ticker callback observes a consistent timeline.
//
// Fake is safe for concurrent use. Channels are buffered with capacity 1 and
// sends are non-blocking, exactly like time.Ticker: a tick that nobody is
// waiting for is dropped rather than queued.
type Fake struct {
	mu      sync.Mutex
	cond    *sync.Cond
	now     time.Time
	waiters []*waiter
}

type waiter struct {
	deadline time.Time
	period   time.Duration // > 0 for tickers, 0 for one-shot timers
	ch       chan time.Time
	stopped  bool
}

// NewFake returns a Fake clock positioned at now.
func NewFake(now time.Time) *Fake {
	f := &Fake{now: now}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// Now reports the fake's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// After returns a channel that receives once the fake advances past d.
func (f *Fake) After(d time.Duration) <-chan time.Time {
	return f.addWaiter(d, 0).ch
}

// NewTimer returns a one-shot timer.
func (f *Fake) NewTimer(d time.Duration) Timer {
	return &fakeTimer{f: f, w: f.addWaiter(d, 0)}
}

// NewTicker returns a ticker that fires every d of fake time.
func (f *Fake) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: non-positive interval for NewTicker")
	}
	return &fakeTicker{f: f, w: f.addWaiter(d, d)}
}

func (f *Fake) addWaiter(d time.Duration, period time.Duration) *waiter {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &waiter{deadline: f.now.Add(d), period: period, ch: make(chan time.Time, 1)}
	f.waiters = append(f.waiters, w)
	f.cond.Broadcast()
	return w
}

// Advance moves the clock forward by d, firing every waiter whose deadline is
// reached, earliest first. A ticker rearms after each fire, so advancing by
// several periods delivers at most one tick per period (excess ticks are
// dropped when the receiver is not listening, as with a real ticker).
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()

	target := f.now.Add(d)
	for {
		next := f.earliestDueLocked(target)
		if next == nil {
			break
		}
		f.now = next.deadline
		select {
		case next.ch <- f.now:
		default: // nobody listening; drop, like time.Ticker
		}
		if next.period > 0 {
			next.deadline = next.deadline.Add(next.period)
		} else {
			next.stopped = true
		}
	}
	f.now = target
	f.pruneLocked()
	f.cond.Broadcast()
}

// earliestDueLocked returns the pending waiter with the smallest deadline that
// is not after target, or nil.
func (f *Fake) earliestDueLocked(target time.Time) *waiter {
	var next *waiter
	for _, w := range f.waiters {
		if w.stopped || w.deadline.After(target) {
			continue
		}
		if next == nil || w.deadline.Before(next.deadline) {
			next = w
		}
	}
	return next
}

func (f *Fake) pruneLocked() {
	live := f.waiters[:0]
	for _, w := range f.waiters {
		if !w.stopped {
			live = append(live, w)
		}
	}
	// Clear the tail so stopped waiters are not retained by the backing array.
	for i := len(live); i < len(f.waiters); i++ {
		f.waiters[i] = nil
	}
	f.waiters = live
}

// BlockUntil waits until n waiters (timers or tickers) are pending. Tests use it
// to close the window between "the goroutine under test has not armed its timer
// yet" and Advance, which would otherwise fire nothing.
func (f *Fake) BlockUntil(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.pendingLocked() < n {
		f.cond.Wait()
	}
}

func (f *Fake) pendingLocked() int {
	var n int
	for _, w := range f.waiters {
		if !w.stopped {
			n++
		}
	}
	return n
}

func (f *Fake) stop(w *waiter) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	active := !w.stopped
	w.stopped = true
	f.cond.Broadcast()
	return active
}

func (f *Fake) reset(w *waiter, d time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	active := !w.stopped
	w.stopped = false
	w.deadline = f.now.Add(d)
	// A fired or stopped waiter has been pruned from the list; re-arming it means
	// putting it back, otherwise Advance would never see it again.
	if !f.containsLocked(w) {
		f.waiters = append(f.waiters, w)
	}
	f.cond.Broadcast()
	return active
}

func (f *Fake) containsLocked(w *waiter) bool {
	for _, x := range f.waiters {
		if x == w {
			return true
		}
	}
	return false
}

type fakeTicker struct {
	f *Fake
	w *waiter
}

func (t *fakeTicker) C() <-chan time.Time { return t.w.ch }
func (t *fakeTicker) Stop()               { t.f.stop(t.w) }

type fakeTimer struct {
	f *Fake
	w *waiter
}

func (t *fakeTimer) C() <-chan time.Time        { return t.w.ch }
func (t *fakeTimer) Stop() bool                 { return t.f.stop(t.w) }
func (t *fakeTimer) Reset(d time.Duration) bool { return t.f.reset(t.w, d) }
