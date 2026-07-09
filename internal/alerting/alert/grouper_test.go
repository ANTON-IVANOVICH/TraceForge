package alert

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"metrics-system/internal/clock"
)

var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	testGroupWait      = 30 * time.Second
	testGroupInterval  = 5 * time.Minute
	testRepeatInterval = 4 * time.Hour
)

func testCfg() GroupConfig {
	return GroupConfig{
		GroupBy:          []string{"alertname", "tenant"},
		GroupWait:        testGroupWait,
		GroupInterval:    testGroupInterval,
		RepeatInterval:   testRepeatInterval,
		DefaultReceivers: []string{"default"},
	}
}

func newGrouper(t *testing.T, cfg GroupConfig, outCap int) (*Grouper, *clock.Fake, chan *Group) {
	t.Helper()
	fake := clock.NewFake(baseTime)
	out := make(chan *Group, outCap)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewGrouper(cfg, out, fake, logger), fake, out
}

func makeAlert(name, tenant, host string, status Status, receivers ...string) *Alert {
	labels := map[string]string{"alertname": name, "tenant": tenant}
	if host != "" {
		labels["host"] = host
	}
	a := &Alert{
		RuleID:   name,
		RuleName: name,
		Status:   status,
		Severity: "warning",
		Labels:   labels,
		StartsAt: baseTime,
	}
	a.Fingerprint = Fingerprint(a.RuleID, a.Labels)
	if len(receivers) > 0 {
		a.Receivers = receivers
	}
	return a
}

func recv(t *testing.T, out chan *Group) *Group {
	t.Helper()
	select {
	case g := <-out:
		return g
	default:
		t.Fatalf("expected a group on out, got none")
		return nil
	}
}

func expectNoSend(t *testing.T, out chan *Group) {
	t.Helper()
	if n := len(out); n != 0 {
		t.Fatalf("expected no group on out, got %d", n)
	}
}

func onlyGroup(t *testing.T, g *Grouper) *groupState {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.groups) != 1 {
		t.Fatalf("expected exactly 1 group, got %d", len(g.groups))
	}
	for _, grp := range g.groups {
		return grp
	}
	return nil
}

// Once a resolution has been delivered the alert leaves the group. Otherwise a
// long-lived group accumulates every fingerprint it has ever seen, and keeps
// re-announcing stale resolutions on every repeat.
func TestGrouperPrunesResolvedAlertsAfterFlush(t *testing.T) {
	g, fake, out := newGrouper(t, testCfg(), 8)

	// Two hosts breach; they share a group (group_by is alertname + tenant).
	g.Ingest(makeAlert("CPUHigh", "acme", "web-1", StatusFiring))
	g.Ingest(makeAlert("CPUHigh", "acme", "web-2", StatusFiring))
	fake.Advance(testGroupWait + time.Second)
	g.flushDue()
	if n := len(recv(t, out).Alerts); n != 2 {
		t.Fatalf("first flush carried %d alerts, want 2", n)
	}

	// One resolves. The next flush must still announce the resolution...
	g.Ingest(makeAlert("CPUHigh", "acme", "web-1", StatusResolved))
	fake.Advance(testGroupInterval + time.Second)
	g.flushDue()
	sent := recv(t, out)
	if len(sent.Alerts) != 2 {
		t.Fatalf("flush carried %d alerts, want both (the resolution must be delivered)", len(sent.Alerts))
	}

	// ...and then drop it, leaving only the alert still firing.
	grp := onlyGroup(t, g)
	g.mu.Lock()
	remaining := len(grp.alerts)
	var kept *Alert
	for _, a := range grp.alerts {
		kept = a
	}
	g.mu.Unlock()

	if remaining != 1 {
		t.Fatalf("group retained %d alerts after the resolution was delivered, want 1", remaining)
	}
	if kept.Status != StatusFiring || kept.Labels["host"] != "web-2" {
		t.Fatalf("the wrong alert survived: %+v", kept)
	}
}

// A group whose every alert has resolved disappears once the resolution is out.
func TestGrouperDeletesGroupOnceEverythingResolved(t *testing.T) {
	g, fake, out := newGrouper(t, testCfg(), 8)

	g.Ingest(makeAlert("CPUHigh", "acme", "web-1", StatusFiring))
	fake.Advance(testGroupWait + time.Second)
	g.flushDue()
	recv(t, out)

	g.Ingest(makeAlert("CPUHigh", "acme", "web-1", StatusResolved))
	fake.Advance(testGroupInterval + time.Second)
	g.flushDue()
	if recv(t, out).Status() != StatusResolved {
		t.Fatal("the final flush must report the group resolved")
	}

	g.mu.Lock()
	n := len(g.groups)
	g.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d groups survived, want the incident to be forgotten", n)
	}
}

// A brand-new group waits out GroupWait before its first notification.
func TestGrouperWaitsGroupWait(t *testing.T) {
	g, fake, out := newGrouper(t, testCfg(), 4)

	g.Ingest(makeAlert("HighLatency", "acme", "web-1", StatusFiring))

	fake.Advance(testGroupWait - time.Second)
	g.flushDue()
	expectNoSend(t, out)

	fake.Advance(time.Second)
	g.flushDue()

	grp := recv(t, out)
	if len(grp.Alerts) != 1 {
		t.Fatalf("want 1 alert, got %d", len(grp.Alerts))
	}
	if grp.Receiver != "default" {
		t.Fatalf("want receiver default, got %q", grp.Receiver)
	}
}

// Two alerts arriving inside the GroupWait window coalesce into one group.
func TestGrouperCoalescesWithinGroupWait(t *testing.T) {
	g, fake, out := newGrouper(t, testCfg(), 4)

	g.Ingest(makeAlert("HighLatency", "acme", "web-1", StatusFiring))
	fake.Advance(10 * time.Second)
	g.Ingest(makeAlert("HighLatency", "acme", "web-2", StatusFiring))

	fake.Advance(testGroupWait - 10*time.Second)
	g.flushDue()

	grp := recv(t, out)
	expectNoSend(t, out)
	if len(grp.Alerts) != 2 {
		t.Fatalf("want 2 alerts in one group, got %d", len(grp.Alerts))
	}
}

// After the first flush, a new member pulls the next flush in to
// LastFlush+GroupInterval — and no sooner.
func TestGrouperPullsNextFlushToGroupInterval(t *testing.T) {
	g, fake, out := newGrouper(t, testCfg(), 4)

	g.Ingest(makeAlert("HighLatency", "acme", "web-1", StatusFiring))
	fake.Advance(testGroupWait)
	g.flushDue()
	_ = recv(t, out)

	g.Ingest(makeAlert("HighLatency", "acme", "web-2", StatusFiring))

	fake.Advance(testGroupInterval - time.Second)
	g.flushDue()
	expectNoSend(t, out) // not before the interval elapses

	fake.Advance(time.Second)
	g.flushDue()
	grp := recv(t, out)
	if len(grp.Alerts) != 2 {
		t.Fatalf("want 2 alerts after pulled flush, got %d", len(grp.Alerts))
	}
}

// An unchanged, still-firing group is re-sent only once RepeatInterval elapses.
func TestGrouperUnchangedResentAtRepeatInterval(t *testing.T) {
	g, fake, out := newGrouper(t, testCfg(), 4)

	g.Ingest(makeAlert("HighLatency", "acme", "web-1", StatusFiring))
	fake.Advance(testGroupWait)
	g.flushDue()
	_ = recv(t, out)

	fake.Advance(testGroupInterval)
	g.flushDue()
	expectNoSend(t, out) // GroupInterval is not a reason to re-send an unchanged group

	fake.Advance(testRepeatInterval - testGroupInterval)
	g.flushDue()
	grp := recv(t, out)
	if grp.Status() != StatusFiring {
		t.Fatalf("want firing reminder, got %s", grp.Status())
	}
}

// A pulled-in flush whose content ends up identical to the last send is
// suppressed until the repeat reminder falls due.
func TestGrouperSuppressesUnchangedContent(t *testing.T) {
	g, fake, out := newGrouper(t, testCfg(), 4)

	g.Ingest(makeAlert("HighLatency", "acme", "web-1", StatusFiring))
	fake.Advance(testGroupWait)
	g.flushDue()
	_ = recv(t, out)

	// Flip resolved then back to firing before the pulled flush: content returns
	// to exactly what was last delivered.
	g.Ingest(makeAlert("HighLatency", "acme", "web-1", StatusResolved))
	g.Ingest(makeAlert("HighLatency", "acme", "web-1", StatusFiring))

	fake.Advance(testGroupInterval)
	g.flushDue()
	expectNoSend(t, out)

	grp := onlyGroup(t, g)
	want := baseTime.Add(testGroupWait).Add(testRepeatInterval)
	if !grp.nextFlush.Equal(want) {
		t.Fatalf("suppressed group should reschedule to repeat window %v, got %v", want, grp.nextFlush)
	}
}

// Once every alert in a group resolves, the group is deleted after it flushes.
func TestGrouperDeletesResolvedGroup(t *testing.T) {
	g, fake, out := newGrouper(t, testCfg(), 4)

	g.Ingest(makeAlert("HighLatency", "acme", "web-1", StatusFiring))
	fake.Advance(testGroupWait)
	g.flushDue()
	_ = recv(t, out)

	g.Ingest(makeAlert("HighLatency", "acme", "web-1", StatusResolved))
	fake.Advance(testGroupInterval)
	g.flushDue()

	grp := recv(t, out)
	if grp.Status() != StatusResolved {
		t.Fatalf("want resolved group, got %s", grp.Status())
	}
	if got := g.Groups(); len(got) != 0 {
		t.Fatalf("resolved group should be deleted, still have %d", len(got))
	}
}

// A full output channel defers the flush to the next tick rather than losing it.
func TestGrouperRetriesOnFullOutput(t *testing.T) {
	g, fake, out := newGrouper(t, testCfg(), 1)
	out <- &Group{Key: "filler"} // occupy the single slot

	g.Ingest(makeAlert("HighLatency", "acme", "web-1", StatusFiring))
	fake.Advance(testGroupWait)
	g.flushDue()

	if len(out) != 1 {
		t.Fatalf("send should have been deferred; out has %d", len(out))
	}
	if grp := onlyGroup(t, g); grp.flushed {
		t.Fatalf("deferred group must not be marked flushed")
	}

	<-out // free the channel

	fake.Advance(time.Second)
	g.flushDue()

	grp := recv(t, out)
	if len(grp.Alerts) != 1 || grp.Receiver != "default" {
		t.Fatalf("retry should deliver the original group, got %+v", grp)
	}
	// The successful send happened on the retry tick, not the deferred one.
	if want := baseTime.Add(testGroupWait + time.Second); !grp.UpdatedAt.Equal(want) {
		t.Fatalf("want delivery at retry tick %v, got %v", want, grp.UpdatedAt)
	}
}

// One alert bound for two receivers becomes two independent groups.
func TestGrouperFansOutToReceivers(t *testing.T) {
	g, fake, out := newGrouper(t, testCfg(), 4)

	g.Ingest(makeAlert("HighLatency", "acme", "web-1", StatusFiring, "team-a", "team-b"))
	fake.Advance(testGroupWait)
	g.flushDue()

	got := map[string]int{}
	for i := 0; i < 2; i++ {
		grp := recv(t, out)
		got[grp.Receiver] = len(grp.Alerts)
	}
	expectNoSend(t, out)
	if got["team-a"] != 1 || got["team-b"] != 1 {
		t.Fatalf("want one alert per receiver, got %v", got)
	}
}

// Ingest must snapshot the alert: mutating the caller's struct afterwards must
// not reach into the stored group.
func TestGrouperIngestClones(t *testing.T) {
	g, fake, out := newGrouper(t, testCfg(), 4)

	a := makeAlert("HighLatency", "acme", "web-1", StatusFiring)
	g.Ingest(a)

	a.Labels["host"] = "mutated"
	a.Status = StatusResolved
	a.Value = 999

	fake.Advance(testGroupWait)
	g.flushDue()

	grp := recv(t, out)
	stored := grp.Alerts[0]
	if stored.Labels["host"] != "web-1" {
		t.Fatalf("stored alert label mutated: %q", stored.Labels["host"])
	}
	if stored.Status != StatusFiring {
		t.Fatalf("stored alert status mutated: %s", stored.Status)
	}
}

// Alerts with no receivers, and no default configured, are dropped rather than
// silently grouped into a receiver-less bucket.
func TestGrouperDropsAlertWithoutReceiver(t *testing.T) {
	cfg := testCfg()
	cfg.DefaultReceivers = nil
	g, fake, out := newGrouper(t, cfg, 4)

	g.Ingest(makeAlert("HighLatency", "acme", "web-1", StatusFiring))
	fake.Advance(testGroupWait)
	g.flushDue()

	expectNoSend(t, out)
	if got := g.Groups(); len(got) != 0 {
		t.Fatalf("want no groups, got %d", len(got))
	}
}

func TestSanitizeDefaults(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := sanitize(GroupConfig{}, logger)

	if cfg.GroupWait != defaultGroupWait || cfg.GroupInterval != defaultGroupInterval || cfg.RepeatInterval != defaultRepeatInterval {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if len(cfg.GroupBy) != 2 || cfg.GroupBy[0] != "alertname" || cfg.GroupBy[1] != "tenant" {
		t.Fatalf("default group_by not applied: %v", cfg.GroupBy)
	}
}

// RepeatInterval below GroupInterval is bumped up to it.
func TestSanitizeBumpsRepeatInterval(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := sanitize(GroupConfig{GroupInterval: time.Hour, RepeatInterval: time.Minute}, logger)
	if cfg.RepeatInterval != time.Hour {
		t.Fatalf("want repeat interval bumped to 1h, got %v", cfg.RepeatInterval)
	}
}

// End-to-end through the real Run ticker loop, driven by the fake clock.
func TestGrouperRunFlushesViaTicker(t *testing.T) {
	g, fake, out := newGrouper(t, testCfg(), 4)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		g.Run(ctx)
		close(done)
	}()

	fake.BlockUntil(1) // wait for Run to arm its ticker
	g.Ingest(makeAlert("HighLatency", "acme", "web-1", StatusFiring))
	fake.Advance(testGroupWait)

	select {
	case grp := <-out:
		if len(grp.Alerts) != 1 {
			t.Fatalf("want 1 alert, got %d", len(grp.Alerts))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for group flush")
	}

	cancel()
	<-done
}
