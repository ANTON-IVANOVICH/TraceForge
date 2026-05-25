package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMetricTypeString(t *testing.T) {
	tests := []struct {
		name string
		in   MetricType
		want string
	}{
		{name: "gauge", in: MetricTypeGauge, want: "gauge"},
		{name: "counter", in: MetricTypeCounter, want: "counter"},
		{name: "unknown", in: MetricType(99), want: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.String(); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMetricTypeJSON(t *testing.T) {
	data, err := json.Marshal(struct {
		Type MetricType `json:"type"`
	}{Type: MetricTypeGauge})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	if string(data) != `{"type":"gauge"}` {
		t.Fatalf("unexpected json: %s", data)
	}

	var out struct {
		Type MetricType `json:"type"`
	}
	if err := json.Unmarshal([]byte(`{"type":"counter"}`), &out); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if out.Type != MetricTypeCounter {
		t.Fatalf("got %v, want %v", out.Type, MetricTypeCounter)
	}
}

func TestBatchValidate(t *testing.T) {
	batch := Batch{
		AgentID: "agent-a",
		Metrics: []Metric{{
			Name:      "cpu_usage_percent",
			Type:      MetricTypeGauge,
			Value:     44.5,
			Timestamp: time.Now().UTC(),
		}},
	}

	if err := batch.Validate(); err != nil {
		t.Fatalf("validate failed: %v", err)
	}
}
