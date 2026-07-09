package notify

import (
	"context"
	"testing"
	"time"

	"metrics-system/internal/alerting/alert"
	"metrics-system/internal/alerting/inhibit"
	"metrics-system/internal/alerting/notify/receivers"
	"metrics-system/internal/alerting/silence"
	"metrics-system/internal/clock"
)

// recordingReceiver captures the groups it is asked to deliver.
type recordingReceiver struct {
	stubReceiver
	got chan *alert.Group
}

func newRecorder(name string, errs ...error) *recordingReceiver {
	return &recordingReceiver{
		stubReceiver: stubReceiver{name: name, errs: errs},
		got:          make(chan *alert.Group, 16),
	}
}

func (r *recordingReceiver) Send(ctx context.Context, g *alert.Group) error {
	err := r.stubReceiver.Send(ctx, g)
	if err == nil {
		r.got <- g
	}
	return err
}

func firingAlert(name, tenant string, receiverNames ...string) *alert.Alert {
	labels := map[string]string{"alertname": name, "tenant": tenant, "agent_id": "web-1"}
	return &alert.Alert{
		Fingerprint: alert.Fingerprint("rule-"+name, labels),
		RuleName:    name,
		Status:      alert.StatusFiring,
		Severity:    "warning",
		Labels:      labels,
		StartsAt:    epoch,
		Receivers:   receiverNames,
	}
}

// notifierHarness wires a notifier with a fake clock and drives it.
type notifierHarness struct {
	in     chan *alert.Alert
	clk    *clock.Fake
	cancel context.CancelFunc
	done   chan struct{}
}

func newNotifier(t *testing.T, cfg Config, recvs []receivers.Receiver, sil *silence.Silencer, inh *inhibit.Inhibitor) *notifierHarness {
	t.Helper()
	clk := clock.NewFake(epoch)
	in := make(chan *alert.Alert, 16)
	n := New(cfg, in, recvs, sil, inh, clk, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); n.Run(ctx) }()

	h := &notifierHarness{in: in, clk: clk, cancel: cancel, done: done}
	t.Cleanup(func() { cancel(); <-done })
	return h
}

// flush advances the fake clock past the group wait and lets the grouper tick.
func (h *notifierHarness) flush(t *testing.T, groupWait time.Duration) {
	t.Helper()
	// Give the forwarder time to hand the alert to the grouper before the tick.
	time.Sleep(20 * time.Millisecond)
	h.clk.Advance(groupWait + 2*time.Second)
}

func baseConfig() Config {
	return Config{
		GroupBy:          []string{"alertname", "tenant"},
		GroupWait:        30 * time.Second,
		GroupInterval:    5 * time.Minute,
		RepeatInterval:   4 * time.Hour,
		DefaultReceivers: []string{"log"},
		SendTimeout:      time.Second,
	}
}

func expectGroup(t *testing.T, r *recordingReceiver) *alert.Group {
	t.Helper()
	select {
	case g := <-r.got:
		return g
	case <-time.After(3 * time.Second):
		t.Fatalf("receiver %s got no group", r.Name())
		return nil
	}
}

func expectNoGroup(t *testing.T, r *recordingReceiver) {
	t.Helper()
	select {
	case g := <-r.got:
		t.Fatalf("receiver %s unexpectedly got group %s", r.Name(), g.Key)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestNotifierDeliversToDefaultReceiver(t *testing.T) {
	t.Parallel()
	rec := newRecorder("log")
	h := newNotifier(t, baseConfig(), []receivers.Receiver{rec}, nil, nil)

	h.in <- firingAlert("CPUHigh", "tenant-a")
	h.flush(t, 30*time.Second)

	g := expectGroup(t, rec)
	if len(g.Alerts) != 1 || g.Alerts[0].Labels["alertname"] != "CPUHigh" {
		t.Fatalf("group = %+v", g)
	}
	if g.Receiver != "log" {
		t.Fatalf("receiver = %q, want the default", g.Receiver)
	}
}

// One alert bound for two receivers becomes two groups, so a slow email cannot
// hold up Slack.
func TestNotifierRoutesToEachReceiver(t *testing.T) {
	t.Parallel()
	a := newRecorder("a")
	b := newRecorder("b")
	h := newNotifier(t, baseConfig(), []receivers.Receiver{a, b}, nil, nil)

	h.in <- firingAlert("CPUHigh", "tenant-a", "a", "b")
	h.flush(t, 30*time.Second)

	ga, gb := expectGroup(t, a), expectGroup(t, b)
	if ga.Receiver != "a" || gb.Receiver != "b" {
		t.Fatalf("receivers = %q, %q", ga.Receiver, gb.Receiver)
	}
}

func TestNotifierRespectsSilence(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	sil := silence.NewSilencer(clk)
	m, err := silence.NewMatcher("agent_id", silence.MatchEqual, "web-1")
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	if err := sil.Set(&silence.Silence{
		ID: "s1", TenantID: "tenant-a", Matchers: []silence.Matcher{m},
		StartsAt: epoch.Add(-time.Hour), EndsAt: epoch.Add(time.Hour),
	}); err != nil {
		t.Fatalf("set silence: %v", err)
	}

	rec := newRecorder("log")
	h := newNotifier(t, baseConfig(), []receivers.Receiver{rec}, sil, nil)

	h.in <- firingAlert("CPUHigh", "tenant-a")
	h.flush(t, 30*time.Second)
	expectNoGroup(t, rec)

	// The silence belongs to tenant-a, so tenant-b's identical alert still fires.
	h.in <- firingAlert("CPUHigh", "tenant-b")
	h.flush(t, 30*time.Second)
	if g := expectGroup(t, rec); g.Labels["tenant"] != "tenant-b" {
		t.Fatalf("group tenant = %q, want tenant-b", g.Labels["tenant"])
	}
}

// HostDown on an agent suppresses CPUHigh on the same agent.
func TestNotifierRespectsInhibition(t *testing.T) {
	t.Parallel()
	source, _ := silence.NewMatcher("alertname", silence.MatchEqual, "HostDown")
	target, _ := silence.NewMatcher("alertname", silence.MatchEqual, "CPUHigh")
	inh := inhibit.New([]inhibit.Rule{{
		SourceMatchers: []silence.Matcher{source},
		TargetMatchers: []silence.Matcher{target},
		Equal:          []string{"agent_id"},
	}})

	rec := newRecorder("log")
	h := newNotifier(t, baseConfig(), []receivers.Receiver{rec}, nil, inh)

	h.in <- firingAlert("HostDown", "tenant-a")
	h.flush(t, 30*time.Second)
	if g := expectGroup(t, rec); g.Alerts[0].Labels["alertname"] != "HostDown" {
		t.Fatalf("expected the HostDown group first")
	}

	h.in <- firingAlert("CPUHigh", "tenant-a")
	h.flush(t, 30*time.Second)
	expectNoGroup(t, rec)
}

// A resolution for an alert nobody was told about must not be announced.
func TestNotifierSuppressesUnannouncedResolution(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	sil := silence.NewSilencer(clk)
	m, _ := silence.NewMatcher("alertname", silence.MatchEqual, "CPUHigh")
	if err := sil.Set(&silence.Silence{
		ID: "s1", Matchers: []silence.Matcher{m},
		StartsAt: epoch.Add(-time.Hour), EndsAt: epoch.Add(time.Hour),
	}); err != nil {
		t.Fatalf("set silence: %v", err)
	}

	rec := newRecorder("log")
	h := newNotifier(t, baseConfig(), []receivers.Receiver{rec}, sil, nil)

	a := firingAlert("CPUHigh", "tenant-a")
	h.in <- a
	h.flush(t, 30*time.Second)
	expectNoGroup(t, rec)

	end := epoch.Add(time.Minute)
	resolved := a.Clone()
	resolved.Status = alert.StatusResolved
	resolved.EndsAt = &end
	h.in <- resolved
	h.flush(t, 30*time.Second)
	expectNoGroup(t, rec)
}

// A transient failure lands in the retry queue and is eventually delivered.
func TestNotifierRetriesTransientFailure(t *testing.T) {
	t.Parallel()
	rec := newRecorder("log", errBoom)

	cfg := baseConfig()
	cfg.Retry = RetryPolicy{MaxAttempts: 3, InitialInterval: time.Second, MaxInterval: time.Minute, Multiplier: 2}
	h := newNotifier(t, cfg, []receivers.Receiver{rec}, nil, nil)

	h.in <- firingAlert("CPUHigh", "tenant-a")
	h.flush(t, 30*time.Second)

	// First attempt fails; the retry fires once its backoff elapses.
	waitFor(t, func() bool { return rec.count() >= 1 })
	h.clk.Advance(2 * time.Second)

	g := expectGroup(t, rec)
	if g.Receiver != "log" {
		t.Fatalf("receiver = %q", g.Receiver)
	}
	if rec.count() != 2 {
		t.Fatalf("receiver called %d times, want 2 (a failure then a retry)", rec.count())
	}
}

// A permanent failure is dropped without a retry.
func TestNotifierDropsPermanentFailure(t *testing.T) {
	t.Parallel()
	rec := newRecorder("log", receivers.Permanent(errBoom))
	h := newNotifier(t, baseConfig(), []receivers.Receiver{rec}, nil, nil)

	h.in <- firingAlert("CPUHigh", "tenant-a")
	h.flush(t, 30*time.Second)

	waitFor(t, func() bool { return rec.count() == 1 })
	h.clk.Advance(10 * time.Minute)
	time.Sleep(50 * time.Millisecond)

	if rec.count() != 1 {
		t.Fatalf("a permanent failure was retried (%d calls)", rec.count())
	}
}

// The dashboard tap sees every alert, including those that are later silenced.
func TestNotifierObserverSeesEveryAlert(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	sil := silence.NewSilencer(clk)
	m, _ := silence.NewMatcher("alertname", silence.MatchEqual, "CPUHigh")
	_ = sil.Set(&silence.Silence{
		ID: "s1", Matchers: []silence.Matcher{m},
		StartsAt: epoch.Add(-time.Hour), EndsAt: epoch.Add(time.Hour),
	})

	rec := newRecorder("log")
	in := make(chan *alert.Alert, 4)
	n := New(baseConfig(), in, []receivers.Receiver{rec}, sil, nil, clk, quietLogger())

	seen := make(chan *alert.Alert, 4)
	n.SetObserver(func(a *alert.Alert) { seen <- a })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); n.Run(ctx) }()
	defer func() { cancel(); <-done }()

	in <- firingAlert("CPUHigh", "tenant-a")
	select {
	case a := <-seen:
		if a.Name() != "CPUHigh" {
			t.Fatalf("observer saw %q", a.Name())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("observer saw nothing — a silenced alert must still reach the dashboard")
	}
}

// A silence created after an alert was already grouped must still stop the
// notification: silencing at ingest alone would let the grouper keep reminding
// receivers about an incident the operator has explicitly muted.
func TestNotifierAppliesSilenceAtDeliveryTime(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(epoch)
	sil := silence.NewSilencer(clk)

	rec := newRecorder("log")
	in := make(chan *alert.Alert, 4)
	n := New(baseConfig(), in, []receivers.Receiver{rec}, sil, nil, clk, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); n.Run(ctx) }()
	defer func() { cancel(); <-done }()

	// The alert is ingested and grouped while nothing is silenced.
	in <- firingAlert("CPUHigh", "tenant-a")
	time.Sleep(20 * time.Millisecond)

	// Only now is the silence created — before the group's first flush.
	m, err := silence.NewMatcher("agent_id", silence.MatchEqual, "web-1")
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	if err := sil.Set(&silence.Silence{
		ID: "s1", Matchers: []silence.Matcher{m},
		StartsAt: epoch.Add(-time.Hour), EndsAt: epoch.Add(time.Hour),
	}); err != nil {
		t.Fatalf("set silence: %v", err)
	}

	clk.Advance(32 * time.Second) // past group_wait
	expectNoGroup(t, rec)

	if rec.count() != 0 {
		t.Fatalf("receiver was called %d times for a silenced group", rec.count())
	}
}

// An unknown receiver name must be logged and dropped, not panic.
func TestNotifierDropsUnknownReceiver(t *testing.T) {
	t.Parallel()
	rec := newRecorder("log")
	h := newNotifier(t, baseConfig(), []receivers.Receiver{rec}, nil, nil)

	h.in <- firingAlert("CPUHigh", "tenant-a", "does-not-exist")
	h.flush(t, 30*time.Second)
	expectNoGroup(t, rec)
}
