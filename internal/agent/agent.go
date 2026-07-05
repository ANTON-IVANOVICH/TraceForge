package agent

import (
	"context"
	"log/slog"
	"sync"
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

	metrics := a.collectAll(tickCtx)
	if len(metrics) == 0 {
		return
	}

	batch := model.Batch{AgentID: a.id, Metrics: metrics}
	if err := a.transport.Send(tickCtx, batch); err != nil {
		a.logger.Error("send failed", "error", err, "count", len(metrics))
		return
	}

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
			a.logger.Warn("collector failed", "name", r.name, "error", r.err)
			continue
		}
		all = append(all, r.metrics...)
	}
	return all
}
