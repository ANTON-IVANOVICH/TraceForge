package alert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"metrics-system/internal/clock"
)

// Default grouping timers, applied when a GroupConfig field is left zero. They
// match Alertmanager's defaults: coalesce for 30s, then no more than one update
// every 5m, and re-announce an unchanged incident every 4h.
const (
	defaultGroupWait      = 30 * time.Second
	defaultGroupInterval  = 5 * time.Minute
	defaultRepeatInterval = 4 * time.Hour
)

// defaultGroupBy is the fallback grouping when none is configured: one
// notification per alert name per tenant, so a tenant's "HighLatency" firing on
// twenty hosts is one incident rather than twenty.
var defaultGroupBy = []string{"alertname", "tenant"}

// GroupConfig controls how alerts are batched into the units a receiver
// delivers, and the three timers that decide when each batch is sent.
type GroupConfig struct {
	// GroupBy is the label set that defines a group. Alerts sharing these label
	// values (for the same receiver) coalesce into one notification.
	GroupBy []string `json:"group_by"`

	// GroupWait delays the first notification for a new group, so a burst of
	// related alerts arrives as one message instead of a storm.
	GroupWait time.Duration `json:"-"`

	// GroupInterval is the floor between successive notifications for a group
	// once it has fired: new or changed alerts pull the next send in, but never
	// closer than this.
	GroupInterval time.Duration `json:"-"`

	// RepeatInterval is how often an unchanged, still-firing group is
	// re-announced as a reminder. It must be >= GroupInterval.
	RepeatInterval time.Duration `json:"-"`

	// DefaultReceivers routes alerts that carry no receivers of their own.
	DefaultReceivers []string `json:"-"`
}

// sanitize fills zero fields with defaults and enforces the one cross-field
// invariant: a repeat interval shorter than the group interval would let the
// reminder fire faster than the "pull-in on change" floor, which is incoherent.
func sanitize(cfg GroupConfig, logger *slog.Logger) GroupConfig {
	if len(cfg.GroupBy) == 0 {
		cfg.GroupBy = append([]string(nil), defaultGroupBy...)
	}
	if cfg.GroupWait <= 0 {
		cfg.GroupWait = defaultGroupWait
	}
	if cfg.GroupInterval <= 0 {
		cfg.GroupInterval = defaultGroupInterval
	}
	if cfg.RepeatInterval <= 0 {
		cfg.RepeatInterval = defaultRepeatInterval
	}
	if cfg.RepeatInterval < cfg.GroupInterval {
		logger.Warn("repeat_interval below group_interval; bumping to group_interval",
			"repeat_interval", cfg.RepeatInterval, "group_interval", cfg.GroupInterval)
		cfg.RepeatInterval = cfg.GroupInterval
	}
	return cfg
}

// Grouper batches alerts into groups and schedules each group's delivery. It is
// the aggregation half of the dispatcher: rule evaluation feeds it single
// alerts through Ingest, and it emits deduplicated Groups on an output channel
// on the schedule set by the three timers.
//
// Ingest may be called from any goroutine while Run owns the flush loop; the
// group table is guarded by a mutex because both writers touch it.
type Grouper struct {
	cfg    GroupConfig
	out    chan<- *Group
	clk    clock.Clock
	logger *slog.Logger

	mu     sync.Mutex
	groups map[string]*groupState
}

// groupState is the mutable per-group bookkeeping the flush loop reasons over.
// alerts is keyed by fingerprint so re-ingesting the same alert dedups rather
// than piling up duplicates.
type groupState struct {
	key      string
	receiver string
	labels   map[string]string
	alerts   map[string]*Alert

	nextFlush time.Time // when this group is next eligible to be sent
	lastFlush time.Time // when it was last successfully sent (zero until then)
	sentHash  string    // content hash of the last successful send
	flushed   bool      // whether it has ever been sent
}

// NewGrouper returns a Grouper that emits ready groups on out. out is sent to
// non-blockingly, so it should be buffered; a send that would block is retried
// on the next tick rather than dropped.
func NewGrouper(cfg GroupConfig, out chan<- *Group, clk clock.Clock, logger *slog.Logger) *Grouper {
	if logger == nil {
		logger = slog.Default()
	}
	if clk == nil {
		clk = clock.New()
	}
	return &Grouper{
		cfg:    sanitize(cfg, logger),
		out:    out,
		clk:    clk,
		logger: logger,
		groups: make(map[string]*groupState),
	}
}

// Ingest routes an alert into one group per receiver and schedules that group.
// It stores a Clone of the alert: the evaluator reuses its alert structs across
// evaluations, so holding the caller's pointer would let a later mutation
// silently rewrite an already-grouped alert.
func (g *Grouper) Ingest(a *Alert) {
	if a == nil {
		return
	}
	recvs := a.Receivers
	if len(recvs) == 0 {
		recvs = g.cfg.DefaultReceivers
	}
	if len(recvs) == 0 {
		g.logger.Warn("dropping alert with no receiver",
			"fingerprint", a.Fingerprint, "rule", a.RuleName, "alert", a.Name())
		return
	}

	now := g.clk.Now()

	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range recvs {
		g.ingestLocked(r, a, now)
	}
}

func (g *Grouper) ingestLocked(receiver string, a *Alert, now time.Time) {
	key := g.groupKey(receiver, a.Labels)

	grp := g.groups[key]
	if grp == nil {
		grp = &groupState{
			key:       key,
			receiver:  receiver,
			labels:    g.groupLabels(a.Labels),
			alerts:    map[string]*Alert{a.Fingerprint: a.Clone()},
			nextFlush: now.Add(g.cfg.GroupWait),
		}
		g.groups[key] = grp
		return
	}

	// A change is a new member or a status flip; a mere value update is not,
	// because it neither warrants pulling the next send in nor breaks dedup.
	old, existed := grp.alerts[a.Fingerprint]
	changed := !existed || old.Status != a.Status
	grp.alerts[a.Fingerprint] = a.Clone()

	// Before the first flush the group is already waiting out GroupWait, so a
	// change just joins that pending batch. Once flushed, a change pulls the
	// next send in — but never faster than GroupInterval since the last one.
	if changed && grp.flushed {
		floor := grp.lastFlush.Add(g.cfg.GroupInterval)
		if floor.Before(now) {
			floor = now
		}
		grp.nextFlush = floor
	}
}

// Run drives the flush loop, checking every second which groups are due and
// emitting them. It blocks until ctx is cancelled.
func (g *Grouper) Run(ctx context.Context) {
	ticker := g.clk.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			g.flushDue()
		}
	}
}

// flushDue sends every group whose NextFlush has arrived. Groups are visited in
// key order so that, when several fall due on the same tick, their emission
// order is deterministic.
func (g *Grouper) flushDue() {
	now := g.clk.Now()

	g.mu.Lock()
	defer g.mu.Unlock()

	keys := make([]string, 0, len(g.groups))
	for k := range g.groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		grp := g.groups[k]
		if grp.nextFlush.After(now) {
			continue
		}
		g.flushGroup(grp, now)
	}
}

// flushGroup emits one due group, unless nothing has changed since the last
// send and the repeat reminder is not yet due. On a successful send it advances
// the timers; a group whose alerts are now all resolved is deleted, because the
// incident is over and there is nothing left to remind about.
func (g *Grouper) flushGroup(grp *groupState, now time.Time) {
	hash := grp.contentHash()

	// Suppress: identical content already delivered, and the repeat window has
	// not elapsed. Reschedule to that window's end so we do not re-check every
	// tick until then.
	if grp.flushed && grp.sentHash == hash && now.Before(grp.lastFlush.Add(g.cfg.RepeatInterval)) {
		grp.nextFlush = grp.lastFlush.Add(g.cfg.RepeatInterval)
		return
	}

	// Non-blocking send: dropping a notification silently is unacceptable, so a
	// full output channel leaves the timers untouched and the group is retried
	// on the next tick.
	select {
	case g.out <- grp.snapshot(now):
	default:
		g.logger.Warn("group flush deferred: output channel full",
			"group", grp.key, "receiver", grp.receiver, "alerts", len(grp.alerts))
		return
	}

	grp.lastFlush = now
	grp.nextFlush = now.Add(g.cfg.RepeatInterval)
	grp.sentHash = hash
	grp.flushed = true

	// A resolution has now been delivered, so the alert leaves the group. Keeping
	// resolved members would make a long-lived group accumulate every fingerprint
	// it ever saw, and would re-announce old resolutions on every repeat.
	for fp, a := range grp.alerts {
		if a.Status == StatusResolved {
			delete(grp.alerts, fp)
		}
	}
	if len(grp.alerts) == 0 {
		delete(g.groups, grp.key) // the incident is over
	}
}

// Groups returns a deterministic deep-copied snapshot of the current groups,
// ordered by key, for the API and tests.
func (g *Grouper) Groups() []*Group {
	now := g.clk.Now()

	g.mu.Lock()
	defer g.mu.Unlock()

	keys := make([]string, 0, len(g.groups))
	for k := range g.groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]*Group, 0, len(keys))
	for _, k := range keys {
		out = append(out, g.groups[k].snapshot(now))
	}
	return out
}

// groupKey identifies a group as its receiver plus the sorted k=v pairs of the
// group-by labels. Sorting the keys makes the identity independent of the
// configured order, and a missing label contributes an empty value so alerts
// that simply lack a group-by label still coalesce together.
func (g *Grouper) groupKey(receiver string, labels map[string]string) string {
	keys := append([]string(nil), g.cfg.GroupBy...)
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(receiver)
	b.WriteByte('|')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	return b.String()
}

// groupLabels is the subset of an alert's labels that identifies its group,
// used for display. Empty values are omitted so a group keyed on a label the
// alert lacks is not shown carrying a blank label.
func (g *Grouper) groupLabels(labels map[string]string) map[string]string {
	out := make(map[string]string)
	for _, k := range g.cfg.GroupBy {
		if v := labels[k]; v != "" {
			out[k] = v
		}
	}
	return out
}

// snapshot deep-copies the group into the payload sent on the output channel,
// with alerts cloned and sorted so downstream holders never observe the
// grouper's live state and every payload is byte-for-byte reproducible.
func (s *groupState) snapshot(now time.Time) *Group {
	alerts := make([]*Alert, 0, len(s.alerts))
	for _, a := range s.alerts {
		alerts = append(alerts, a.Clone())
	}
	SortAlerts(alerts)
	return &Group{
		Key:       s.key,
		Receiver:  s.receiver,
		Labels:    CloneLabels(s.labels),
		Alerts:    alerts,
		UpdatedAt: now,
	}
}

// contentHash summarises what a receiver would care about: which alerts are
// present and whether each is firing or resolved. Fingerprints are sorted first
// so the hash is stable across map iteration order, letting an unchanged group
// be recognised and suppressed until its repeat reminder falls due.
func (s *groupState) contentHash() string {
	fps := make([]string, 0, len(s.alerts))
	for fp := range s.alerts {
		fps = append(fps, fp)
	}
	sort.Strings(fps)

	h := sha256.New()
	for _, fp := range fps {
		h.Write([]byte(fp))
		h.Write([]byte{0})
		h.Write([]byte(s.alerts[fp].Status))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
