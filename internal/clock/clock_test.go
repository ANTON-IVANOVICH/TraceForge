package clock

import (
	"sync"
	"testing"
	"time"
)

var epoch = time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

func TestFakeNowOnlyMovesOnAdvance(t *testing.T) {
	f := NewFake(epoch)
	if got := f.Now(); !got.Equal(epoch) {
		t.Fatalf("Now() = %v, want %v", got, epoch)
	}
	f.Advance(90 * time.Second)
	if got := f.Now(); !got.Equal(epoch.Add(90 * time.Second)) {
		t.Fatalf("Now() = %v, want %v", got, epoch.Add(90*time.Second))
	}
}

func TestFakeAfterFiresOnlyOnceDeadlinePassed(t *testing.T) {
	f := NewFake(epoch)
	ch := f.After(time.Minute)

	f.Advance(59 * time.Second)
	select {
	case v := <-ch:
		t.Fatalf("fired early at %v", v)
	default:
	}

	f.Advance(time.Second)
	select {
	case v := <-ch:
		if !v.Equal(epoch.Add(time.Minute)) {
			t.Fatalf("fired with %v, want the deadline %v", v, epoch.Add(time.Minute))
		}
	default:
		t.Fatal("did not fire once the deadline was reached")
	}
}

// A single Advance spanning several periods must fire the ticker chronologically
// rather than jumping straight to the target: code reading Now() from a tick has
// to observe the tick's own instant.
func TestFakeTickerFiresAtItsDeadline(t *testing.T) {
	f := NewFake(epoch)
	tk := f.NewTicker(time.Second)
	defer tk.Stop()

	var seen []time.Time
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3; i++ {
			seen = append(seen, <-tk.C())
		}
	}()

	for i := 0; i < 3; i++ {
		f.Advance(time.Second)
		// Let the consumer drain the tick before the next Advance, otherwise the
		// non-blocking send drops it, exactly as a real ticker would.
		time.Sleep(time.Millisecond)
	}
	<-done

	for i, got := range seen {
		want := epoch.Add(time.Duration(i+1) * time.Second)
		if !got.Equal(want) {
			t.Fatalf("tick %d = %v, want %v", i, got, want)
		}
	}
}

func TestFakeTickerStopSilencesIt(t *testing.T) {
	f := NewFake(epoch)
	tk := f.NewTicker(time.Second)
	tk.Stop()

	f.Advance(10 * time.Second)
	select {
	case v := <-tk.C():
		t.Fatalf("stopped ticker fired at %v", v)
	default:
	}
}

func TestFakeTimerStopAndReset(t *testing.T) {
	f := NewFake(epoch)
	tm := f.NewTimer(time.Minute)

	if !tm.Stop() {
		t.Fatal("Stop on a pending timer should report it was active")
	}
	f.Advance(2 * time.Minute)
	select {
	case <-tm.C():
		t.Fatal("stopped timer fired")
	default:
	}

	tm.Reset(time.Minute)
	f.Advance(time.Minute)
	select {
	case <-tm.C():
	default:
		t.Fatal("timer did not fire after Reset")
	}
}

// Advancing past several distinct deadlines must fire them in chronological
// order, not in registration order.
func TestFakeAdvanceFiresInChronologicalOrder(t *testing.T) {
	f := NewFake(epoch)
	late := f.After(3 * time.Second)
	early := f.After(1 * time.Second)

	f.Advance(5 * time.Second)

	e := <-early
	l := <-late
	if !e.Before(l) {
		t.Fatalf("early fired at %v, late at %v — want early first", e, l)
	}
	if !e.Equal(epoch.Add(time.Second)) || !l.Equal(epoch.Add(3*time.Second)) {
		t.Fatalf("deadlines not preserved: early=%v late=%v", e, l)
	}
}

// BlockUntil closes the race between "the goroutine under test has not armed its
// timer yet" and Advance, which would otherwise fire nothing.
func TestFakeBlockUntil(t *testing.T) {
	f := NewFake(epoch)
	fired := make(chan time.Time, 1)

	go func() {
		fired <- <-f.After(time.Minute)
	}()

	f.BlockUntil(1)
	f.Advance(time.Minute)

	select {
	case v := <-fired:
		if !v.Equal(epoch.Add(time.Minute)) {
			t.Fatalf("fired at %v, want %v", v, epoch.Add(time.Minute))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the waiter to fire")
	}
}

func TestFakeConcurrentUse(t *testing.T) {
	f := NewFake(epoch)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tk := f.NewTicker(time.Second)
			defer tk.Stop()
			_ = f.Now()
			<-tk.C()
		}()
	}

	// Every ticker buffers one tick, so a single Advance releases all of them.
	f.BlockUntil(8)
	f.Advance(time.Second)

	if waitTimeout(&wg, 5*time.Second) {
		t.Fatal("not every ticker fired")
	}
}

// waitTimeout reports whether wg is still pending after d.
func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return false
	case <-time.After(d):
		return true
	}
}

func TestRealClock(t *testing.T) {
	c := New()
	start := c.Now()

	tk := c.NewTicker(time.Millisecond)
	defer tk.Stop()
	<-tk.C()

	tm := c.NewTimer(time.Millisecond)
	<-tm.C()

	<-c.After(time.Millisecond)

	if !c.Now().After(start) {
		t.Fatal("real clock did not advance")
	}
}
