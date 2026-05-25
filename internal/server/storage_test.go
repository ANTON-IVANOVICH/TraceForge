package server

import (
	"testing"
	"time"

	"metrics-system/internal/model"
)

func TestStorageReturnsCopy(t *testing.T) {
	s := NewStorage()
	s.Add(model.Batch{
		AgentID: "agent-a",
		Metrics: []model.Metric{{
			Name:      "cpu_usage_percent",
			Type:      model.MetricTypeGauge,
			Value:     12,
			Timestamp: time.Now().UTC(),
		}},
	})

	all := s.All()
	if len(all) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(all))
	}

	all[0].Name = "mutated"
	all2 := s.All()
	if all2[0].Name != "cpu_usage_percent" {
		t.Fatalf("storage leaked mutable slice")
	}
}
