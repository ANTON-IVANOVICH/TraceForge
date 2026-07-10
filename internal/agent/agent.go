package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"metrics-system/internal/model"
)

// Transport ships a batch to the server. It abstracts over the wire protocol:
// there is an HTTP implementation (Sender) and a gRPC one (GRPCSender).
type Transport interface {
	Send(ctx context.Context, batch model.Batch) error
	Close() error
}

type Agent struct {
	id         string
	interval   time.Duration
	collectors []Collector
	transport  Transport
	logger     *slog.Logger

	stats Stats
}

// Stats counts what the agent has done. The counters are monotonic; a rate is
// the query layer's job.
//
// They exist so the agent can answer /metrics and /readyz about itself. An agent
// whose collectors all fail still ships an empty batch to nowhere and logs at
// debug — silently, forever. `collect_failures_total` is the number that says so.
type Stats struct {
	ticks           atomic.Uint64
	collected       atomic.Uint64
	collectFailures atomic.Uint64
	batchesSent     atomic.Uint64
	sendFailures    atomic.Uint64

	// lastCollectUnixNano is the moment the last tick produced at least one
	// metric. Readiness reads it; see Agent.Ready.
	lastCollectUnixNano atomic.Int64
}

// Snapshot is the JSON- and Prometheus-facing view of Stats.
type Snapshot struct {
	Ticks           uint64    `json:"ticks"`
	Collected       uint64    `json:"collected"`
	CollectFailures uint64    `json:"collect_failures"`
	BatchesSent     uint64    `json:"batches_sent"`
	SendFailures    uint64    `json:"send_failures"`
	LastCollect     time.Time `json:"last_collect"`
}

// Stats returns a consistent-enough snapshot: each counter is read atomically,
// and they are not read under one lock, because a scrape does not need the five
// of them to agree to the microsecond.
func (a *Agent) Stats() Snapshot {
	var last time.Time
	if ns := a.stats.lastCollectUnixNano.Load(); ns != 0 {
		last = time.Unix(0, ns).UTC()
	}
	return Snapshot{
		Ticks:           a.stats.ticks.Load(),
		Collected:       a.stats.collected.Load(),
		CollectFailures: a.stats.collectFailures.Load(),
		BatchesSent:     a.stats.batchesSent.Load(),
		SendFailures:    a.stats.sendFailures.Load(),
		LastCollect:     last,
	}
}

// Ready reports whether the agent has collected anything recently.
//
// It deliberately does not consider whether the *server* is reachable. The agent
// is a DaemonSet: nothing routes traffic to it, so its readiness only gates
// rollouts. An agent that reported "not ready" whenever the server was down would
// stall every DaemonSet rollout precisely during a server outage — the moment you
// most want to be able to deploy a fix. What readiness means here is "this
// process is doing its own job": it has collected metrics within a few intervals.
func (a *Agent) Ready(_ context.Context) error {
	ns := a.stats.lastCollectUnixNano.Load()
	if ns == 0 {
		if a.stats.ticks.Load() == 0 {
			return errors.New("no collection tick has run yet")
		}
		return errors.New("every collector has failed since start")
	}
	// Three intervals: one to be slow, one to be unlucky, one for the probe's own
	// timing. A single interval would flap on a busy host.
	if age := time.Since(time.Unix(0, ns)); age > 3*a.interval {
		return fmt.Errorf("last successful collection was %s ago", age.Round(time.Second))
	}
	return nil
}

func New(id string, interval time.Duration, collectors []Collector, transport Transport, logger *slog.Logger) *Agent {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Agent{
		id:         id,
		interval:   interval,
		collectors: collectors,
		transport:  transport,
		logger:     logger,
	}
}

func (a *Agent) Run(ctx context.Context) error {
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	// Release the transport (close the gRPC stream/connection) on the way out.
	defer func() {
		if err := a.transport.Close(); err != nil {
			a.logger.Warn("transport close failed", "error", err)
		}
	}()

	a.logger.Info("agent started", "id", a.id, "interval", a.interval.String(), "collectors", len(a.collectors))

	for {
		select {
		case <-ctx.Done():
			a.logger.Info("agent stopping", "reason", ctx.Err())
			return nil
		case <-ticker.C:
			a.tick(ctx)
		}
	}
}

func (a *Agent) tick(ctx context.Context) {
	tickCtx, cancel := context.WithTimeout(ctx, a.interval)
	defer cancel()

	a.stats.ticks.Add(1)

	metrics := a.collectAll(tickCtx)
	if len(metrics) == 0 {
		return
	}
	a.stats.collected.Add(uint64(len(metrics)))
	// Recorded before the send, not after: readiness asks whether this process is
	// collecting, and a server that is down is not a reason to call the agent
	// unhealthy. See Agent.Ready.
	a.stats.lastCollectUnixNano.Store(time.Now().UnixNano())

	batch := model.Batch{AgentID: a.id, Metrics: metrics}
	if err := a.transport.Send(tickCtx, batch); err != nil {
		a.stats.sendFailures.Add(1)
		a.logger.Error("send failed", "error", err, "count", len(metrics))
		return
	}
	a.stats.batchesSent.Add(1)

	a.logger.Debug("batch sent", "count", len(metrics))
}

func (a *Agent) collectAll(ctx context.Context) []model.Metric {
	type result struct {
		name    string
		metrics []model.Metric
		err     error
	}

	results := make(chan result, len(a.collectors))
	var wg sync.WaitGroup

	for _, collector := range a.collectors {
		wg.Add(1)
		go func(c Collector) {
			defer wg.Done()
			items, err := c.Collect(ctx)
			results <- result{name: c.Name(), metrics: items, err: err}
		}(collector)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	all := make([]model.Metric, 0, len(a.collectors)*2)
	for r := range results {
		if r.err != nil {
			a.stats.collectFailures.Add(1)
			a.logger.Warn("collector failed", "name", r.name, "error", r.err)
			continue
		}
		all = append(all, r.metrics...)
	}
	return all
}
