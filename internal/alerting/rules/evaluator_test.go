package rules

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"metrics-system/internal/alerting/alert"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// evalHarness drives one rule against a mutable querier.
type evalHarness struct {
	t     *testing.T
	q     *fakeQuerier
	eval  *Evaluator
	out   chan *alert.Alert
	rule  *Rule
	clock time.Time
}

func newHarness(t *testing.T, expr string, forDur time.Duration) *evalHarness {
	t.Helper()
	q := &fakeQuerier{series: map[string][]Series{}}
	out := make(chan *alert.Alert, 32)
	rule := &Rule{
		ID:         "r1",
		Name:       "CPUHigh",
		Severity:   SeverityWarning,
		For:        Duration(forDur),
		Interval:   Duration(15 * time.Second),
		Enabled:    true,
		Expression: expr,
	}
	if err := rule.Compile(); err != nil {
		t.Fatalf("compile: %v", err)
	}
	return &evalHarness{
		t:     t,
		q:     q,
		eval:  NewEvaluator(NewMemoryStateStore(), out, quietLogger()),
		out:   out,
		rule:  rule,
		clock: evalAt,
	}
}

// set replaces the cpu series (empty removes it, making the condition go away).
func (h *evalHarness) set(values ...float64) {
	if len(values) == 0 {
		h.q.series["cpu"] = nil
		return
	}
	h.q.series["cpu"] = []Series{series(lbl("host", "a"), values...)}
}

func (h *evalHarness) advance(d time.Duration) { h.clock = h.clock.Add(d) }

func (h *evalHarness) evaluate() {
	h.t.Helper()
	if err := h.eval.Evaluate(context.Background(), h.rule, h.q, h.clock); err != nil {
		h.t.Fatalf("evaluate: %v", err)
	}
}

// drain returns every alert emitted since the last call.
func (h *evalHarness) drain() []*alert.Alert {
	var out []*alert.Alert
	for {
		select {
		case a := <-h.out:
			out = append(out, a)
		default:
			return out
		}
	}
}

func (h *evalHarness) state() *AlertState {
	h.t.Helper()
	states, err := h.eval.states.GetByRule(h.rule.ID)
	if err != nil {
		h.t.Fatalf("states: %v", err)
	}
	if len(states) == 0 {
		return nil
	}
	return states[0]
}

// The whole point of `for`: a momentary spike must not page anyone.
func TestForSemanticsDelaysFiring(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "cpu > 90", 5*time.Minute)
	h.set(95)

	h.evaluate()
	if got := h.drain(); len(got) != 0 {
		t.Fatalf("alerted immediately: %v", got)
	}
	if st := h.state(); st == nil || st.State != StatePending {
		t.Fatalf("state = %v, want pending", st)
	}

	// Still inside the `for` window.
	h.advance(4 * time.Minute)
	h.evaluate()
	if got := h.drain(); len(got) != 0 {
		t.Fatalf("alerted before `for` elapsed: %v", got)
	}

	h.advance(time.Minute)
	h.evaluate()
	got := h.drain()
	if len(got) != 1 || got[0].Status != alert.StatusFiring {
		t.Fatalf("got %d alerts, want one firing", len(got))
	}
	if got[0].Value != 95 {
		t.Fatalf("alert value = %v, want 95", got[0].Value)
	}
	if st := h.state(); st.State != StateFiring || st.FiredAt == nil {
		t.Fatalf("state = %v, want firing with FiredAt set", st)
	}
}

func TestForZeroFiresImmediately(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "cpu > 90", 0)
	h.set(95)

	h.evaluate()
	if got := h.drain(); len(got) != 1 || got[0].Status != alert.StatusFiring {
		t.Fatalf("got %d alerts, want one firing on the first evaluation", len(got))
	}
}

// A condition that lapses and returns restarts its `for` window: the alert must
// hold continuously, not cumulatively.
func TestPendingResetsWhenConditionLapses(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "cpu > 90", 5*time.Minute)

	h.set(95)
	h.evaluate() // pending
	h.advance(4 * time.Minute)

	h.set(10) // condition gone; the pending state is forgotten silently
	h.evaluate()
	if got := h.drain(); len(got) != 0 {
		t.Fatalf("a pending alert that never fired must not notify: %v", got)
	}
	if st := h.state(); st != nil {
		t.Fatalf("pending state survived the condition lapsing: %v", st)
	}

	h.set(95)
	h.evaluate() // pending again, clock restarted
	h.advance(4 * time.Minute)
	h.evaluate()
	if got := h.drain(); len(got) != 0 {
		t.Fatalf("`for` did not restart: %v", got)
	}
}

func TestResolvedIsEmittedAndResurrects(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "cpu > 90", 0)

	h.set(95)
	h.evaluate()
	firing := h.drain()
	if len(firing) != 1 {
		t.Fatalf("want one firing alert, got %d", len(firing))
	}

	h.advance(time.Minute)
	h.set(10)
	h.evaluate()
	resolved := h.drain()
	if len(resolved) != 1 || resolved[0].Status != alert.StatusResolved {
		t.Fatalf("want one resolved alert, got %v", resolved)
	}
	if resolved[0].EndsAt == nil {
		t.Fatal("resolved alert must carry EndsAt")
	}
	if resolved[0].Fingerprint != firing[0].Fingerprint {
		t.Fatal("resolution must reuse the firing alert's fingerprint")
	}

	// Within the grace period the same fingerprint comes back to life.
	h.advance(time.Minute)
	h.set(95)
	h.evaluate()
	again := h.drain()
	if len(again) != 1 || again[0].Status != alert.StatusFiring {
		t.Fatalf("resurrection did not fire: %v", again)
	}
	if again[0].Fingerprint != firing[0].Fingerprint {
		t.Fatal("a resurrected alert must keep its fingerprint")
	}
}

func TestResolvedStateIsGarbageCollected(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "cpu > 90", 0)
	h.set(95)
	h.evaluate()
	h.set(10)
	h.evaluate()
	if st := h.state(); st == nil || st.State != StateResolved {
		t.Fatalf("state = %v, want resolved", st)
	}

	h.advance(resolvedGrace + time.Minute)
	h.evaluate()
	if st := h.state(); st != nil {
		t.Fatalf("resolved state outlived its grace period: %v", st)
	}
}

// A firing alert is re-announced periodically, but not on every evaluation —
// that would flood the channel between evaluation and notification.
func TestResendDelayThrottlesRefiring(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "cpu > 90", 0)
	h.eval.ResendDelay = 10 * time.Minute
	h.set(95)

	h.evaluate()
	if len(h.drain()) != 1 {
		t.Fatal("want the initial firing alert")
	}

	h.advance(time.Minute)
	h.evaluate()
	if got := h.drain(); len(got) != 0 {
		t.Fatalf("re-emitted inside the resend delay: %v", got)
	}

	h.advance(10 * time.Minute)
	h.evaluate()
	if got := h.drain(); len(got) != 1 {
		t.Fatalf("did not re-emit after the resend delay: %d alerts", len(got))
	}
}

// The fingerprint must not depend on map iteration order, or dedup breaks and
// every evaluation looks like a brand-new alert.
func TestFingerprintIsStable(t *testing.T) {
	t.Parallel()
	a := alert.Fingerprint("r1", map[string]string{"host": "a", "tenant": "x", "alertname": "N"})
	for i := 0; i < 50; i++ {
		b := alert.Fingerprint("r1", map[string]string{"tenant": "x", "alertname": "N", "host": "a"})
		if a != b {
			t.Fatalf("fingerprint changed across iterations: %s != %s", a, b)
		}
	}
	if c := alert.Fingerprint("r2", map[string]string{"host": "a", "tenant": "x", "alertname": "N"}); c == a {
		t.Fatal("different rules must produce different fingerprints")
	}
}

func TestAlertLabelsAndAnnotations(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "cpu > 90", 0)
	h.rule.Labels = map[string]string{"team": "core"}
	h.rule.Annotations = map[string]string{
		"summary": "CPU is {{ .Value }}% on {{ .Labels.host }}",
		"broken":  "{{ .Nope",
	}
	h.rule.Receivers = []string{"log"}
	h.set(95)
	h.evaluate()

	got := h.drain()
	if len(got) != 1 {
		t.Fatalf("want one alert, got %d", len(got))
	}
	a := got[0]
	if a.Labels["alertname"] != "CPUHigh" || a.Labels["severity"] != "warning" || a.Labels["team"] != "core" {
		t.Fatalf("labels = %v", a.Labels)
	}
	if want := "CPU is 95% on a"; a.Annotations["summary"] != want {
		t.Fatalf("summary = %q, want %q", a.Annotations["summary"], want)
	}
	// A broken template must not break alerting; the raw text passes through.
	if a.Annotations["broken"] != "{{ .Nope" {
		t.Fatalf("broken annotation = %q, want the raw text", a.Annotations["broken"])
	}
	if len(a.Receivers) != 1 || a.Receivers[0] != "log" {
		t.Fatalf("receivers = %v", a.Receivers)
	}
}

// Rule labels override the derived alertname/severity.
func TestRuleLabelsWin(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "cpu > 90", 0)
	h.rule.Labels = map[string]string{"alertname": "Custom", "severity": "critical"}
	h.set(95)
	h.evaluate()

	a := h.drain()[0]
	if a.Labels["alertname"] != "Custom" || a.Labels["severity"] != "critical" {
		t.Fatalf("labels = %v, want the rule's explicit values", a.Labels)
	}
}

// The tenant label is server-controlled. A rule must not be able to forge it,
// or its alerts would be delivered to, and silenceable by, another tenant.
func TestTenantLabelCannotBeForged(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "cpu > 90", 0)
	h.rule.TenantID = "tenant-a"
	// Bypass Compile's rejection to prove the evaluator itself re-stamps the label.
	h.rule.Labels = map[string]string{"tenant": "tenant-b"}
	h.set(95)
	h.evaluate()

	a := h.drain()[0]
	if a.Labels["tenant"] != "tenant-a" {
		t.Fatalf("alert tenant = %q, want the rule's owner tenant-a", a.Labels["tenant"])
	}
}

// An aggregation drops the labels it does not group by — including `tenant`.
// Re-stamping it is what keeps the alert visible to the tenant that owns it.
func TestTenantLabelSurvivesAggregation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "max by (host) (cpu) > 90", 0)
	h.rule.TenantID = "tenant-a"
	h.q.series["cpu"] = []Series{series(lbl("host", "a", "tenant", "tenant-a"), 95)}
	h.evaluate()

	a := h.drain()[0]
	if a.Labels["tenant"] != "tenant-a" {
		t.Fatalf("alert labels = %v, want tenant restored after aggregation", a.Labels)
	}
}

// With auth off there is no tenant; the label must not linger from a rule.
func TestTenantLabelStrippedWhenUnscoped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "cpu > 90", 0)
	h.rule.Labels = map[string]string{"tenant": "sneaky"}
	h.set(95)
	h.evaluate()

	if _, ok := h.drain()[0].Labels["tenant"]; ok {
		t.Fatal("an unscoped rule produced a tenant label")
	}
}

// A rule may not declare the reserved tenant label at all.
func TestCompileRejectsReservedTenantLabel(t *testing.T) {
	t.Parallel()
	r := &Rule{ID: "a", Name: "N", Expression: "cpu > 1", Labels: map[string]string{"tenant": "b"}}
	if err := r.Compile(); err == nil {
		t.Fatal("a rule declaring the reserved tenant label was accepted")
	}
}

// Two samples in one vector can collapse to a single fingerprint once the rule's
// labels overwrite whatever told them apart. That must produce one alert, not two.
func TestDuplicateFingerprintInOneVectorFiresOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "cpu > 90", 0)
	h.rule.Labels = map[string]string{"host": "collapsed"}
	h.q.series["cpu"] = []Series{
		series(lbl("host", "a"), 95),
		series(lbl("host", "b"), 99),
	}
	h.evaluate()

	got := h.drain()
	if len(got) != 1 {
		t.Fatalf("emitted %d alerts for two samples sharing a fingerprint, want 1", len(got))
	}
	states, _ := h.eval.states.GetByRule(h.rule.ID)
	if len(states) != 1 {
		t.Fatalf("stored %d states, want 1", len(states))
	}
}

// Emission must respect the evaluation deadline instead of blocking forever on
// a saturated channel.
func TestEmitHonoursContextCancellation(t *testing.T) {
	t.Parallel()
	out := make(chan *alert.Alert) // unbuffered, nobody reading
	e := NewEvaluator(NewMemoryStateStore(), out, quietLogger())

	rule := &Rule{ID: "r", Name: "N", Expression: "cpu > 1", Severity: SeverityWarning, Enabled: true}
	if err := rule.Compile(); err != nil {
		t.Fatalf("compile: %v", err)
	}
	q := &fakeQuerier{series: map[string][]Series{"cpu": {series(lbl("host", "a"), 5)}}}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := e.Evaluate(ctx, rule, q, evalAt); err == nil {
		t.Fatal("expected Evaluate to fail once the context expired")
	}
}

func TestEvaluateRejectsUncompiledRule(t *testing.T) {
	t.Parallel()
	e := NewEvaluator(NewMemoryStateStore(), make(chan *alert.Alert, 1), quietLogger())
	err := e.Evaluate(context.Background(), &Rule{ID: "r"}, &fakeQuerier{}, evalAt)
	if err == nil {
		t.Fatal("an uncompiled rule must not evaluate")
	}
}
