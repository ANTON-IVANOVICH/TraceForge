package notify

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"metrics-system/internal/clock"
)

var epoch = time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

var errBoom = errors.New("boom")

func fail() error { return errBoom }
func ok() error   { return nil }

func TestCircuitOpensAfterConsecutiveFailures(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	cb := NewCircuitBreaker(3, 1, time.Minute, clk)

	var calls atomic.Int32
	failing := func() error { calls.Add(1); return errBoom }

	for i := 0; i < 3; i++ {
		if err := cb.Call(failing); !errors.Is(err, errBoom) {
			t.Fatalf("attempt %d: err = %v, want the underlying failure", i, err)
		}
	}
	if cb.State() != StateOpen {
		t.Fatalf("state = %s, want open", cb.State())
	}

	// The fourth call must be rejected without touching the backend at all —
	// that is the whole point: a dead server stops accumulating stuck callers.
	if err := cb.Call(failing); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("backend called %d times, want 3 (the open circuit must short-circuit)", got)
	}
}

// A success resets the consecutive-failure count; the breaker trips on a run of
// failures, not on a lifetime total.
func TestCircuitSuccessResetsFailureRun(t *testing.T) {
	t.Parallel()
	cb := NewCircuitBreaker(3, 1, time.Minute, clock.NewFake(epoch))

	_ = cb.Call(fail)
	_ = cb.Call(fail)
	_ = cb.Call(ok)
	_ = cb.Call(fail)
	_ = cb.Call(fail)

	if cb.State() != StateClosed {
		t.Fatalf("state = %s, want closed", cb.State())
	}
}

func TestCircuitHalfOpenClosesOnSuccess(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	cb := NewCircuitBreaker(1, 2, time.Minute, clk)

	_ = cb.Call(fail)
	if cb.State() != StateOpen {
		t.Fatalf("state = %s, want open", cb.State())
	}

	// Still cooling down.
	clk.Advance(59 * time.Second)
	if err := cb.Call(ok); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err = %v, want the circuit to stay open", err)
	}

	clk.Advance(2 * time.Second)
	if err := cb.Call(ok); err != nil { // first probe
		t.Fatalf("probe failed: %v", err)
	}
	if cb.State() != StateHalfOpen {
		t.Fatalf("state = %s, want half-open after one of two successes", cb.State())
	}
	if err := cb.Call(ok); err != nil { // second probe closes it
		t.Fatalf("probe failed: %v", err)
	}
	if cb.State() != StateClosed {
		t.Fatalf("state = %s, want closed", cb.State())
	}
}

func TestCircuitHalfOpenReopensOnFailure(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	cb := NewCircuitBreaker(1, 1, time.Minute, clk)

	_ = cb.Call(fail)
	clk.Advance(2 * time.Minute)

	if err := cb.Call(fail); !errors.Is(err, errBoom) {
		t.Fatalf("probe err = %v, want the underlying failure", err)
	}
	if cb.State() != StateOpen {
		t.Fatalf("state = %s, want open again", cb.State())
	}
	// The cooldown restarts from the new trip.
	clk.Advance(30 * time.Second)
	if err := cb.Call(ok); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err = %v, want the cooldown to restart", err)
	}
}

// While half-open only one probe may be in flight: a burst of alerts must not
// all pile onto a backend that is only just recovering.
func TestCircuitHalfOpenAdmitsOneProbe(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	cb := NewCircuitBreaker(1, 5, time.Minute, clk)

	_ = cb.Call(fail)
	clk.Advance(2 * time.Minute)

	var inFlight, rejected, admitted atomic.Int32
	release := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := cb.Call(func() error {
				admitted.Add(1)
				inFlight.Add(1)
				<-release // hold the probe open while the others try
				inFlight.Add(-1)
				return nil
			})
			if errors.Is(err, ErrCircuitOpen) {
				rejected.Add(1)
			}
		}()
	}

	// Let the callers pile up, then release the single probe.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && rejected.Load() < 7 {
		time.Sleep(time.Millisecond)
	}
	if got := admitted.Load(); got != 1 {
		t.Fatalf("%d probes admitted concurrently, want exactly 1", got)
	}
	close(release)
	wg.Wait()

	if got := rejected.Load(); got != 7 {
		t.Fatalf("%d callers rejected, want 7", got)
	}
}

func TestCircuitStateString(t *testing.T) {
	t.Parallel()
	for state, want := range map[CircuitState]string{
		StateClosed: "closed", StateOpen: "open", StateHalfOpen: "half-open", CircuitState(9): "unknown",
	} {
		if got := state.String(); got != want {
			t.Fatalf("String() = %q, want %q", got, want)
		}
	}
}

// The breaker is on the hot path of every delivery, so it must be race-free.
func TestCircuitConcurrentCalls(t *testing.T) {
	t.Parallel()
	cb := NewCircuitBreaker(5, 2, 10*time.Millisecond, clock.New())

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = cb.Call(func() error {
					if (i+j)%3 == 0 {
						return errBoom
					}
					return nil
				})
				_ = cb.State()
			}
		}(i)
	}
	wg.Wait()
}
