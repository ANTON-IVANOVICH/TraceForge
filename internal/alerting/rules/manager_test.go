package rules

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"metrics-system/internal/alerting/alert"
	"metrics-system/internal/clock"
)

// countingQuerier records how many evaluations reached storage.
type countingQuerier struct {
	fakeQuerier
	calls atomic.Int32
}

func (q *countingQuerier) Instant(ctx context.Context, name string, matchers map[string]string, at time.Time) (Vector, error) {
	q.calls.Add(1)
	return q.fakeQuerier.Instant(ctx, name, matchers, at)
}

func newManagerHarness(t *testing.T, clk clock.Clock) (*Manager, RuleStore, *countingQuerier, chan *alert.Alert) {
	t.Helper()
	q := &countingQuerier{fakeQuerier: fakeQuerier{series: map[string][]Series{
		"cpu": {series(lbl("host", "a"), 95)},
	}}}
	store := NewMemoryRuleStore()
	out := make(chan *alert.Alert, 64)
	m := NewManager(store, func(string) Querier { return q }, NewMemoryStateStore(), out, clk, quietLogger())
	return m, store, q, out
}

// eventually polls cond until it holds or the deadline expires — a hang fails
// the test rather than blocking the suite forever.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestManagerEvaluatesOnItsInterval(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(evalAt)
	m, store, q, out := newManagerHarness(t, clk)

	rule := compiledRule(t, "r1", "")
	rule.Interval = Duration(time.Minute)
	if err := store.Put(rule); err != nil {
		t.Fatalf("put: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	// The runner first sleeps out its start jitter, which is < interval.
	clk.BlockUntil(1)
	clk.Advance(time.Minute)
	eventually(t, "the first evaluation", func() bool { return q.calls.Load() >= 1 })

	select {
	case a := <-out:
		if a.Status != alert.StatusFiring {
			t.Fatalf("status = %s", a.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no alert emitted")
	}
}

// A disabled rule must never be scheduled.
func TestManagerSkipsDisabledRule(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(evalAt)
	m, store, q, _ := newManagerHarness(t, clk)

	rule := compiledRule(t, "r1", "")
	rule.Enabled = false
	rule.Interval = Duration(time.Minute)
	if err := store.Put(rule); err != nil {
		t.Fatalf("put: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	clk.Advance(10 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	if n := q.calls.Load(); n != 0 {
		t.Fatalf("a disabled rule was evaluated %d times", n)
	}
}

// Apply must replace a rule's runner, never leave two racing to evaluate it.
func TestManagerApplyReplacesRunner(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(evalAt)
	m, _, q, _ := newManagerHarness(t, clk)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	rule := compiledRule(t, "r1", "")
	rule.Interval = Duration(time.Minute)
	for i := 0; i < 5; i++ {
		m.Apply(rule)
	}

	// Exactly one runner is armed, so exactly one waiter is pending on the clock.
	clk.BlockUntil(1)
	clk.Advance(time.Minute)
	eventually(t, "one evaluation", func() bool { return q.calls.Load() >= 1 })

	// Give any duplicate runner a chance to fire a second time.
	time.Sleep(20 * time.Millisecond)
	if n := q.calls.Load(); n > 1 {
		t.Fatalf("%d evaluations after one tick — a duplicate runner survived Apply", n)
	}

	// Disabling the rule stops its runner.
	disabled := rule.Clone()
	disabled.Enabled = false
	m.Apply(disabled)

	before := q.calls.Load()
	clk.Advance(10 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	if q.calls.Load() != before {
		t.Fatal("a disabled rule kept evaluating")
	}
}

func TestManagerRemoveClearsState(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(evalAt)
	m, store, _, _ := newManagerHarness(t, clk)

	rule := compiledRule(t, "r1", "")
	rule.Interval = Duration(time.Minute)
	if err := store.Put(rule); err != nil {
		t.Fatalf("put: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	if err := m.states.Put(&AlertState{RuleID: "r1", Fingerprint: "f", State: StateFiring}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	m.Remove(rule)

	if got, _ := m.states.GetByRule("r1"); len(got) != 0 {
		t.Fatal("Remove left alert state behind")
	}
}

// Deleting a rule that is currently firing must announce the resolution, or the
// notifier's group is never emptied and keeps reminding receivers forever.
func TestManagerRemoveResolvesFiringAlerts(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(evalAt)
	m, _, _, out := newManagerHarness(t, clk)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	rule := compiledRule(t, "r1", "")
	if err := m.states.Put(&AlertState{
		RuleID: "r1", Fingerprint: "f", State: StateFiring,
		Labels: lbl("alertname", "Nr1"), ActiveAt: evalAt,
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	m.Remove(rule)

	select {
	case a := <-out:
		if a.Status != alert.StatusResolved {
			t.Fatalf("status = %s, want resolved", a.Status)
		}
		if a.EndsAt == nil {
			t.Fatal("resolved alert must carry EndsAt")
		}
	default:
		t.Fatal("removing a firing rule emitted no resolution")
	}
	if got, _ := m.states.GetByRule("r1"); len(got) != 0 {
		t.Fatal("Remove left alert state behind")
	}
}

// Disabling a rule is a removal as far as its alerts are concerned.
func TestManagerDisableResolvesFiringAlerts(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(evalAt)
	m, _, _, out := newManagerHarness(t, clk)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	rule := compiledRule(t, "r1", "")
	if err := m.states.Put(&AlertState{
		RuleID: "r1", Fingerprint: "f", State: StateFiring,
		Labels: lbl("alertname", "Nr1"), ActiveAt: evalAt,
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	disabled := rule.Clone()
	disabled.Enabled = false
	m.Apply(disabled)

	select {
	case a := <-out:
		if a.Status != alert.StatusResolved {
			t.Fatalf("status = %s, want resolved", a.Status)
		}
	default:
		t.Fatal("disabling a firing rule emitted no resolution")
	}
}

func TestManagerActiveAlertsIsTenantScoped(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(evalAt)
	m, _, _, _ := newManagerHarness(t, clk)

	seed := []*AlertState{
		{RuleID: "r1", Fingerprint: "f1", State: StateFiring, Labels: lbl("tenant", "a")},
		{RuleID: "r1", Fingerprint: "f2", State: StatePending, Labels: lbl("tenant", "a")},
		{RuleID: "r2", Fingerprint: "f3", State: StateFiring, Labels: lbl("tenant", "b")},
		{RuleID: "r2", Fingerprint: "f4", State: StateResolved, Labels: lbl("tenant", "a")},
	}
	for _, st := range seed {
		if err := m.states.Put(st); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if got := m.ActiveAlerts("a"); len(got) != 2 {
		t.Fatalf("tenant a sees %d active alerts, want 2 (firing + pending, no resolved, no tenant b)", len(got))
	}
	if got := m.ActiveAlerts("b"); len(got) != 1 {
		t.Fatalf("tenant b sees %d active alerts, want 1", len(got))
	}
	if got := m.ActiveAlerts(""); len(got) != 3 {
		t.Fatalf("unscoped sees %d active alerts, want 3", len(got))
	}
}

// Stop must be idempotent, terminate, and be safe to race against Apply.
func TestManagerStopIsIdempotentAndRaceFree(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(evalAt)
	m, _, _, _ := newManagerHarness(t, clk)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	rule := compiledRule(t, "r1", "")
	rule.Interval = Duration(time.Minute)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Apply(rule)
		}()
	}
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			m.Stop()
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop deadlocked against concurrent Apply")
	}

	m.Stop() // still safe afterwards
}

// After Stop, Apply must not resurrect a runner.
func TestManagerApplyAfterStopIsNoop(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(evalAt)
	m, _, q, _ := newManagerHarness(t, clk)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Stop()

	rule := compiledRule(t, "r1", "")
	rule.Interval = Duration(time.Minute)
	m.Apply(rule)

	clk.Advance(10 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	if n := q.calls.Load(); n != 0 {
		t.Fatalf("a rule applied after Stop evaluated %d times", n)
	}
}

// The start jitter must land inside [0, interval): synchronised rules would
// otherwise turn steady read load into a periodic burst against storage.
func TestRandomDelayIsWithinInterval(t *testing.T) {
	t.Parallel()
	const interval = time.Minute
	for i := 0; i < 1000; i++ {
		d := randomDelay(interval)
		if d < 0 || d >= interval {
			t.Fatalf("randomDelay = %v, want [0, %v)", d, interval)
		}
	}
	if randomDelay(0) != 0 {
		t.Fatal("randomDelay(0) must be 0")
	}
}
