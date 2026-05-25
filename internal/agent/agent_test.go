package agent

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"metrics-system/internal/model"
)

type stubCollector struct {
	name string
	out  []model.Metric
	err  error
}

func (s stubCollector) Name() string { return s.name }

func (s stubCollector) Collect(context.Context) ([]model.Metric, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.out, nil
}

func TestCollectAll(t *testing.T) {
	a := &Agent{
		collectors: []Collector{
			stubCollector{name: "ok-1", out: []model.Metric{{Name: "m1", Type: model.MetricTypeGauge, Timestamp: time.Now().UTC()}}},
			stubCollector{name: "err", err: errors.New("boom")},
			stubCollector{name: "ok-2", out: []model.Metric{{Name: "m2", Type: model.MetricTypeGauge, Timestamp: time.Now().UTC()}}},
		},
		logger: slog.Default(),
	}

	metrics := a.collectAll(context.Background())
	if len(metrics) != 2 {
		t.Fatalf("expected 2 metrics, got %d", len(metrics))
	}
}
