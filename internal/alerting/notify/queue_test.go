package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"metrics-system/internal/alerting/alert"
	"metrics-system/internal/alerting/notify/receivers"
	"metrics-system/internal/clock"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// stubReceiver records deliveries and returns scripted errors.
type stubReceiver struct {
	name string

	mu    sync.Mutex
	calls int
	errs  []error // consumed in order; nil once exhausted
}

func (r *stubReceiver) Name() string { return r.name }

func (r *stubReceiver) Send(context.Context, *alert.Group) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if len(r.errs) == 0 {
		return nil
	}
	err := r.errs[0]
	r.errs = r.errs[1:]
	return err
}

func (r *stubReceiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func testGroup() *alert.Group {
	return &alert.Group{Key: "log|alertname=X", Receiver: "log", Alerts: []*alert.Alert{{Fingerprint: "f1"}}}
}

func TestNextDelayIsExponentialAndCapped(t *testing.T) {
	t.Parallel()
	p := RetryPolicy{MaxAttempts: 10, InitialInterval: time.Second, MaxInterval: 10 * time.Second, Multiplier: 2, Jitter: 0}

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	for i, w := range want {
		if got := p.NextDelay(i + 1); got != w {
			t.Fatalf("NextDelay(%d) = %v, want %v", i+1, got, w)
		}
	}
	// Attempt numbers below 1 are clamped rather than producing a fractional delay.
	if got := p.NextDelay(0); got != time.Second {
		t.Fatalf("NextDelay(0) = %v, want the initial interval", got)
	}
}

// Jitter must stay inside its band and never go negative — a negative delay
// would make the retry fire immediately, defeating the backoff.
func TestNextDelayJitterStaysInBand(t *testing.T) {
	t.Parallel()
	p := RetryPolicy{MaxAttempts: 5, InitialInterval: time.Second, MaxInterval: time.Minute, Multiplier: 2, Jitter: 0.3}

	var sawLow, sawHigh bool
	for i := 0; i < 2000; i++ {
		d := p.NextDelay(1)
		if d < 700*time.Millisecond || d > 1300*time.Millisecond {
			t.Fatalf("NextDelay = %v, outside the ±30%% band around 1s", d)
		}
		if d < 900*time.Millisecond {
			sawLow = true
		}
		if d > 1100*time.Millisecond {
			sawHigh = true
		}
	}
	if !sawLow || !sawHigh {
		t.Fatal("jitter did not spread delays in both directions")
	}
}

func TestNextDelayNeverNegative(t *testing.T) {
	t.Parallel()
	p := RetryPolicy{MaxAttempts: 5, InitialInterval: time.Second, MaxInterval: time.Minute, Multiplier: 2, Jitter: 1.5}
	for i := 0; i < 1000; i++ {
		if d := p.NextDelay(1); d < 0 {
			t.Fatalf("NextDelay = %v", d)
		}
	}
}

func TestEnqueueRefusesExhaustedAttempts(t *testing.T) {
	t.Parallel()
	p := RetryPolicy{MaxAttempts: 2, InitialInterval: time.Second, MaxInterval: time.Minute, Multiplier: 2}
	q := NewRetryQueue(p, 10, nil, clock.NewFake(epoch), quietLogger())
	r := &stubReceiver{name: "log"}

	if !q.Enqueue(r, testGroup(), 1) || !q.Enqueue(r, testGroup(), 2) {
		t.Fatal("attempts within the budget must be accepted")
	}
	if q.Enqueue(r, testGroup(), 3) {
		t.Fatal("an attempt beyond MaxAttempts must be refused")
	}
}

// A full queue refuses new work rather than buffering without bound.
func TestEnqueueRefusesWhenFull(t *testing.T) {
	t.Parallel()
	q := NewRetryQueue(DefaultRetryPolicy(), 2, nil, clock.NewFake(epoch), quietLogger())
	r := &stubReceiver{name: "log"}

	for i := 0; i < 2; i++ {
		if !q.Enqueue(r, testGroup(), 1) {
			t.Fatalf("enqueue %d: the first two must fit", i)
		}
	}
	if q.Enqueue(r, testGroup(), 1) {
		t.Fatal("a full queue must refuse")
	}
	if q.Len() != 2 {
		t.Fatalf("Len = %d, want 2", q.Len())
	}
}

// runQueue starts the queue and returns a stop function.
func runQueue(t *testing.T, q *RetryQueue) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); q.Run(ctx) }()
	return cancel, done
}

func TestRetryEventuallySucceeds(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	r := &stubReceiver{name: "log", errs: []error{errBoom, errBoom}}

	var delivered atomic.Bool
	send := func(ctx context.Context, rcv receivers.Receiver, g *alert.Group) error {
		err := rcv.Send(ctx, g)
		if err == nil {
			delivered.Store(true)
		}
		return err
	}
	q := NewRetryQueue(RetryPolicy{MaxAttempts: 5, InitialInterval: time.Second, MaxInterval: time.Minute, Multiplier: 2}, 10, send, clk, quietLogger())

	cancel, done := runQueue(t, q)
	defer func() { cancel(); <-done }()

	clk.BlockUntil(1) // the Run ticker is armed
	q.Enqueue(r, testGroup(), 1)

	// Two failures, then success. Each round: advance past the backoff, then let
	// the tick fire and the delivery goroutine finish.
	for i := 0; i < 3 && !delivered.Load(); i++ {
		clk.Advance(2 * time.Minute)
		waitFor(t, func() bool { return r.count() >= i+1 })
		waitFor(t, func() bool { return q.Len() > 0 || delivered.Load() })
	}

	if !delivered.Load() {
		t.Fatalf("never delivered after %d attempts", r.count())
	}
	if r.count() != 3 {
		t.Fatalf("receiver called %d times, want 3 (two failures then a success)", r.count())
	}
}

// A permanent error is dropped immediately: retrying a 400 cannot help.
func TestRetryDropsPermanentFailure(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	r := &stubReceiver{name: "log", errs: []error{receivers.Permanent(errBoom)}}

	send := func(ctx context.Context, rcv receivers.Receiver, g *alert.Group) error { return rcv.Send(ctx, g) }
	q := NewRetryQueue(DefaultRetryPolicy(), 10, send, clk, quietLogger())

	cancel, done := runQueue(t, q)
	defer func() { cancel(); <-done }()

	clk.BlockUntil(1)
	q.Enqueue(r, testGroup(), 1)
	clk.Advance(2 * time.Second)

	waitFor(t, func() bool { return r.count() == 1 })
	waitFor(t, func() bool { return q.Len() == 0 })

	clk.Advance(10 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	if r.count() != 1 {
		t.Fatalf("a permanent failure was retried (%d calls)", r.count())
	}
}

func TestRetryHonoursMaxAttempts(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	r := &stubReceiver{name: "log", errs: []error{errBoom, errBoom, errBoom, errBoom, errBoom, errBoom}}

	send := func(ctx context.Context, rcv receivers.Receiver, g *alert.Group) error { return rcv.Send(ctx, g) }
	q := NewRetryQueue(RetryPolicy{MaxAttempts: 3, InitialInterval: time.Second, MaxInterval: time.Minute, Multiplier: 2}, 10, send, clk, quietLogger())

	cancel, done := runQueue(t, q)
	defer func() { cancel(); <-done }()

	clk.BlockUntil(1)
	q.Enqueue(r, testGroup(), 1)

	for i := 0; i < 3; i++ {
		clk.Advance(2 * time.Minute)
		waitFor(t, func() bool { return r.count() >= i+1 })
	}
	waitFor(t, func() bool { return q.Len() == 0 })

	clk.Advance(10 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	if got := r.count(); got != 3 {
		t.Fatalf("receiver called %d times, want exactly MaxAttempts (3)", got)
	}
}

// Items come due in time order regardless of insertion order.
func TestQueueOrdersByDueTime(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	p := RetryPolicy{MaxAttempts: 5, InitialInterval: time.Second, MaxInterval: time.Hour, Multiplier: 10}
	q := NewRetryQueue(p, 10, nil, clk, quietLogger())

	late := &stubReceiver{name: "late"}
	early := &stubReceiver{name: "early"}
	q.Enqueue(late, testGroup(), 3)  // 100s
	q.Enqueue(early, testGroup(), 1) // 1s

	due := q.popDue(clk.Now().Add(2 * time.Second))
	if len(due) != 1 || due[0].receiver.Name() != "early" {
		t.Fatalf("popDue returned %d items, want only the early one", len(due))
	}
	if q.Len() != 1 {
		t.Fatalf("Len = %d, want the late item still queued", q.Len())
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for a condition")
}

func TestIsPermanent(t *testing.T) {
	t.Parallel()
	if receivers.IsPermanent(errBoom) {
		t.Fatal("a plain error must be transient")
	}
	if !receivers.IsPermanent(receivers.Permanent(errBoom)) {
		t.Fatal("Permanent must be detected")
	}
	if !errors.Is(receivers.Permanent(errBoom), errBoom) {
		t.Fatal("Permanent must keep the cause unwrappable")
	}
	if receivers.Permanent(nil) != nil {
		t.Fatal("Permanent(nil) must be nil")
	}
	if !receivers.IsPermanent(receivers.Permanentf("bad %d", 400)) {
		t.Fatal("Permanentf must be detected")
	}
}
