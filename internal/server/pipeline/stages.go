package pipeline

import (
	"errors"
	"math"
	"strings"
	"time"

	"metrics-system/internal/model"
)

var (
	errEmptyName = errors.New("empty metric name")
	errBadName   = errors.New("metric name contains whitespace")
	errBadType   = errors.New("unsupported metric type")
	errBadValue  = errors.New("value is NaN or Inf")
)

// unpackStage: ingestCh (batches) -> validateCh (individual metrics), tagging
// each metric with its agent_id label. Runs as a single goroutine.
func (p *Pipeline) unpackStage() {
	for batch := range p.ingestCh {
		for _, m := range batch.Metrics {
			if m.Labels == nil {
				m.Labels = make(map[string]string, 2)
			}
			m.Labels["agent_id"] = batch.AgentID
			// tenant is a server-controlled label: set it from the authenticated
			// principal and strip any client-supplied value, so a client can
			// never write into another tenant. Empty when auth is disabled.
			if batch.Tenant != "" {
				m.Labels["tenant"] = batch.Tenant
			} else {
				delete(m.Labels, "tenant")
			}
			p.validateCh <- m
		}
	}
}

// validateStage drops malformed metrics and forwards the rest to enrichCh.
func (p *Pipeline) validateStage(workerID int) {
	for m := range p.validateCh {
		if err := validate(m); err != nil {
			p.stats.IncInvalid(1)
			p.logger.Debug("invalid metric", "worker", workerID, "error", err, "name", m.Name)
			continue
		}
		p.enrichCh <- m
	}
}

// enrichStage fills in derived fields (currently: a timestamp when the agent
// omitted one) before storage.
func (p *Pipeline) enrichStage(_ int) {
	for m := range p.enrichCh {
		if m.Timestamp.IsZero() {
			m.Timestamp = time.Now().UTC()
		}
		p.storeCh <- m
	}
}

// storeStage batches metrics and flushes them to storage by size or on a short
// timer. Batching turns many tiny writes into few WriteBatch calls — essential
// for transactional backends like bbolt where each commit is costly.
func (p *Pipeline) storeStage(_ int) {
	const maxBatch = 1000
	batch := make([]model.Metric, 0, maxBatch)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := p.storage.WriteBatch(batch); err != nil {
			p.logger.Error("store batch failed", "error", err, "count", len(batch))
		} else {
			p.stats.IncStored(int64(len(batch)))
			if p.onStored != nil {
				p.onStored(batch) // observer copies synchronously if it retains
			}
		}
		batch = batch[:0]
	}

	for {
		select {
		case m, ok := <-p.storeCh:
			if !ok { // storeCh closed on drain: flush the tail and exit
				flush()
				return
			}
			batch = append(batch, m)
			if len(batch) >= maxBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func validate(m model.Metric) error {
	if m.Name == "" {
		return errEmptyName
	}
	if strings.ContainsAny(m.Name, " \t\n") {
		return errBadName
	}
	if m.Type != model.MetricTypeGauge && m.Type != model.MetricTypeCounter {
		return errBadType
	}
	if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
		return errBadValue
	}
	return nil
}
