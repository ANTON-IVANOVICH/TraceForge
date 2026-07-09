// Package notify turns the stream of alerts coming out of rule evaluation into
// notifications actually delivered to receivers. It is the asynchronous,
// failure-tolerant half of alerting: evaluation is deterministic and must never
// miss a tick, while delivery talks to third parties that time out, rate-limit
// and fall over. The two halves are joined by a channel and kept apart by a
// grouper, a retry queue and a circuit breaker per receiver.
package notify

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"metrics-system/internal/alerting/alert"
	"metrics-system/internal/alerting/inhibit"
	"metrics-system/internal/alerting/notify/receivers"
	"metrics-system/internal/alerting/silence"
	"metrics-system/internal/clock"
)

// Config tunes grouping, delivery concurrency, retries and circuit breaking.
// Zero fields take sensible defaults.
type Config struct {
	GroupBy          []string
	GroupWait        time.Duration
	GroupInterval    time.Duration
	RepeatInterval   time.Duration
	DefaultReceivers []string

	Workers     int           // concurrent group deliveries
	QueueSize   int           // buffered groups awaiting delivery
	SendTimeout time.Duration // per-delivery deadline

	Retry RetryPolicy

	FailureThreshold int           // consecutive failures that open a receiver's circuit
	SuccessThreshold int           // half-open successes that close it again
	BreakerTimeout   time.Duration // how long a circuit stays open
}

func (c *Config) withDefaults() {
	if c.Workers <= 0 {
		c.Workers = 8
	}
	if c.QueueSize <= 0 {
		c.QueueSize = 256
	}
	if c.SendTimeout <= 0 {
		c.SendTimeout = 30 * time.Second
	}
	if c.Retry.MaxAttempts <= 0 {
		c.Retry = DefaultRetryPolicy()
	}
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 5
	}
	if c.SuccessThreshold <= 0 {
		c.SuccessThreshold = 1
	}
	if c.BreakerTimeout <= 0 {
		c.BreakerTimeout = time.Minute
	}
}

// Notifier is the dispatcher. Run owns every goroutine it starts and returns
// only once they have all exited.
type Notifier struct {
	grouper   *alert.Grouper
	silencer  *silence.Silencer
	inhibitor *inhibit.Inhibitor
	receivers map[string]receivers.Receiver
	breakers  map[string]*CircuitBreaker
	retry     *RetryQueue

	in     <-chan *alert.Alert
	groups chan *alert.Group

	workers     int
	sendTimeout time.Duration

	observer func(*alert.Alert) // optional tap for the live dashboard

	logger *slog.Logger
}

// New builds a notifier over the given receivers. Each receiver gets its own
// circuit breaker: a dead SMTP server must not stop Slack notifications.
func New(
	cfg Config,
	in <-chan *alert.Alert,
	recvs []receivers.Receiver,
	silencer *silence.Silencer,
	inhibitor *inhibit.Inhibitor,
	clk clock.Clock,
	logger *slog.Logger,
) *Notifier {
	cfg.withDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	if clk == nil {
		clk = clock.New()
	}

	n := &Notifier{
		silencer:    silencer,
		inhibitor:   inhibitor,
		receivers:   make(map[string]receivers.Receiver, len(recvs)),
		breakers:    make(map[string]*CircuitBreaker, len(recvs)),
		in:          in,
		groups:      make(chan *alert.Group, cfg.QueueSize),
		workers:     cfg.Workers,
		sendTimeout: cfg.SendTimeout,
		logger:      logger,
	}
	for _, r := range recvs {
		n.receivers[r.Name()] = r
		n.breakers[r.Name()] = NewCircuitBreaker(cfg.FailureThreshold, cfg.SuccessThreshold, cfg.BreakerTimeout, clk)
	}

	n.grouper = alert.NewGrouper(alert.GroupConfig{
		GroupBy:          cfg.GroupBy,
		GroupWait:        cfg.GroupWait,
		GroupInterval:    cfg.GroupInterval,
		RepeatInterval:   cfg.RepeatInterval,
		DefaultReceivers: cfg.DefaultReceivers,
	}, n.groups, clk, logger)

	n.retry = NewRetryQueue(cfg.Retry, cfg.QueueSize, n.send, clk, logger)
	return n
}

// SetObserver registers a tap that sees every alert entering the notifier —
// before silencing and inhibition, because the dashboard should show what is
// actually wrong, not only what got delivered. Call before Run.
func (n *Notifier) SetObserver(fn func(*alert.Alert)) { n.observer = fn }

// Groups exposes the current group snapshot (for the API and tests).
func (n *Notifier) Groups() []*alert.Group { return n.grouper.Groups() }

// Run starts the grouper, the alert forwarder, the delivery worker pool and the
// retry queue, and blocks until ctx is cancelled and all of them have stopped.
func (n *Notifier) Run(ctx context.Context) {
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		n.grouper.Run(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		n.forwardAlerts(ctx)
	}()

	for i := 0; i < n.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.dispatchWorker(ctx)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		n.retry.Run(ctx)
	}()

	wg.Wait()
}

// forwardAlerts is the single owner of the firing set, so it needs no lock. It
// applies silencing and inhibition before handing an alert to the grouper.
func (n *Notifier) forwardAlerts(ctx context.Context) {
	firing := make(map[string]*alert.Alert)
	// forwarded remembers which alerts actually reached the grouper, so a
	// resolution for an alert that was silenced or inhibited while firing is not
	// announced as "resolved" to a receiver that never heard it was broken.
	forwarded := make(map[string]struct{})

	for {
		select {
		case <-ctx.Done():
			return
		case a, ok := <-n.in:
			if !ok {
				return
			}
			if n.observer != nil {
				n.observer(a)
			}

			if a.Status == alert.StatusFiring {
				firing[a.Fingerprint] = a
			} else {
				delete(firing, a.Fingerprint)
			}

			if a.Status == alert.StatusResolved {
				if _, announced := forwarded[a.Fingerprint]; !announced {
					continue
				}
				delete(forwarded, a.Fingerprint)
				n.grouper.Ingest(a)
				continue
			}

			if n.silencer != nil && n.silencer.Mutes(a) {
				n.logger.Debug("alert silenced", "fingerprint", a.Fingerprint, "alert", a.Name())
				continue
			}
			if n.inhibitor != nil && n.inhibitor.Inhibited(a, firingList(firing)) {
				n.logger.Debug("alert inhibited", "fingerprint", a.Fingerprint, "alert", a.Name())
				continue
			}

			forwarded[a.Fingerprint] = struct{}{}
			n.grouper.Ingest(a)
		}
	}
}

func firingList(firing map[string]*alert.Alert) []*alert.Alert {
	out := make([]*alert.Alert, 0, len(firing))
	for _, a := range firing {
		out = append(out, a)
	}
	return out
}

func (n *Notifier) dispatchWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case g := <-n.groups:
			n.dispatch(ctx, g)
		}
	}
}

// dispatch delivers one group to the single receiver it was routed to. Routing
// happened in the grouper: an alert bound for two receivers became two groups,
// so a slow email receiver cannot delay the Slack one.
func (n *Notifier) dispatch(ctx context.Context, g *alert.Group) {
	r, ok := n.receivers[g.Receiver]
	if !ok {
		n.logger.Warn("unknown receiver, dropping group", "receiver", g.Receiver, "group", g.Key)
		return
	}

	err := n.send(ctx, r, g)
	if err == nil {
		return
	}
	if receivers.IsPermanent(err) {
		n.logger.Error("permanent delivery failure, giving up",
			"receiver", r.Name(), "group", g.Key, "error", err)
		return
	}
	if !n.retry.Enqueue(r, g, 1) {
		n.logger.Error("retry queue full, dropping group",
			"receiver", r.Name(), "group", g.Key, "error", err)
	}
}

// send is the one path through which every delivery attempt — first try and
// retry alike — passes, so the circuit breaker sees the complete failure record.
func (n *Notifier) send(ctx context.Context, r receivers.Receiver, g *alert.Group) error {
	// Silences are re-applied here, not only when an alert is ingested: a silence
	// created after an alert was grouped must stop the repeat reminders too, and a
	// group sitting in the retry queue must not outlive the silence that muted it.
	g = n.withoutSilenced(g)
	if len(g.Alerts) == 0 {
		return nil
	}

	sendCtx, cancel := context.WithTimeout(ctx, n.sendTimeout)
	defer cancel()

	cb := n.breakers[r.Name()]
	if cb == nil {
		return r.Send(sendCtx, g)
	}
	return cb.Call(func() error { return r.Send(sendCtx, g) })
}

// withoutSilenced returns g with its silenced alerts removed. The group is a
// snapshot the grouper handed over, but it is copied anyway so a retry of the
// same group re-evaluates silences from scratch.
func (n *Notifier) withoutSilenced(g *alert.Group) *alert.Group {
	if n.silencer == nil {
		return g
	}
	kept := make([]*alert.Alert, 0, len(g.Alerts))
	for _, a := range g.Alerts {
		if !n.silencer.Mutes(a) {
			kept = append(kept, a)
		}
	}
	if len(kept) == len(g.Alerts) {
		return g
	}
	cp := *g
	cp.Alerts = kept
	return &cp
}
