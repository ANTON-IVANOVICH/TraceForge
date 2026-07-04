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
				m.Labels = make(map[string]string, 1)
			}
			m.Labels["agent_id"] = batch.AgentID
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

// storeStage writes metrics into the storage backend.
func (p *Pipeline) storeStage(_ int) {
	for m := range p.storeCh {
		p.storage.Write(m)
		p.stats.IncStored(1)
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
