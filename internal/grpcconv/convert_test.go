package grpcconv

import (
	"testing"
	"time"

	"metrics-system/internal/model"
	metricspb "metrics-system/internal/proto/metricspb"
)

func TestBatchRoundTrip(t *testing.T) {
	t.Parallel()

	orig := model.Batch{
		AgentID: "agent-1",
		Metrics: []model.Metric{
			{
				Name:      "cpu_usage_percent",
				Type:      model.MetricTypeGauge,
				Value:     42.5,
				Timestamp: time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC),
				Labels:    map[string]string{"host": "web-1", "region": "eu"},
			},
			{
				Name:      "requests_total",
				Type:      model.MetricTypeCounter,
				Value:     1234,
				Timestamp: time.Date(2026, 7, 5, 12, 0, 5, 0, time.UTC),
			},
		},
	}

	got, err := BatchFromProto(BatchToProto(orig))
	if err != nil {
		t.Fatalf("round-trip: %v", err)
	}

	if got.AgentID != orig.AgentID {
		t.Errorf("agent id: got %q want %q", got.AgentID, orig.AgentID)
	}
	if len(got.Metrics) != len(orig.Metrics) {
		t.Fatalf("metric count: got %d want %d", len(got.Metrics), len(orig.Metrics))
	}
	for i := range orig.Metrics {
		o, g := orig.Metrics[i], got.Metrics[i]
		if g.Name != o.Name || g.Type != o.Type || g.Value != o.Value {
			t.Errorf("metric[%d] scalar mismatch: got %+v want %+v", i, g, o)
		}
		if !g.Timestamp.Equal(o.Timestamp) {
			t.Errorf("metric[%d] timestamp: got %v want %v", i, g.Timestamp, o.Timestamp)
		}
		if len(g.Labels) != len(o.Labels) {
			t.Errorf("metric[%d] labels: got %v want %v", i, g.Labels, o.Labels)
		}
		for k, v := range o.Labels {
			if g.Labels[k] != v {
				t.Errorf("metric[%d] label %q: got %q want %q", i, k, g.Labels[k], v)
			}
		}
	}

	// Round-tripped batch must still validate.
	if err := got.Validate(); err != nil {
		t.Errorf("validate after round-trip: %v", err)
	}
}

func TestMetricTypeFromProtoRejectsUnspecified(t *testing.T) {
	t.Parallel()

	if _, err := MetricTypeFromProto(metricspb.MetricType_METRIC_TYPE_UNSPECIFIED); err == nil {
		t.Fatal("expected error for UNSPECIFIED metric type")
	}
}

func TestBatchFromProtoRejectsBadMetricType(t *testing.T) {
	t.Parallel()

	pb := &metricspb.Batch{
		AgentId: "a",
		Metrics: []*metricspb.Metric{
			{Name: "ok", Type: metricspb.MetricType_METRIC_TYPE_GAUGE},
			{Name: "bad", Type: metricspb.MetricType_METRIC_TYPE_UNSPECIFIED},
		},
	}
	if _, err := BatchFromProto(pb); err == nil {
		t.Fatal("expected error for batch with an unspecified metric type")
	}
}

func TestMetricFromProtoNilTimestampIsZero(t *testing.T) {
	t.Parallel()

	m, err := MetricFromProto(&metricspb.Metric{
		Name: "x",
		Type: metricspb.MetricType_METRIC_TYPE_GAUGE,
		// no timestamp
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !m.Timestamp.IsZero() {
		t.Errorf("expected zero timestamp, got %v", m.Timestamp)
	}
	// A zero timestamp must fail model validation.
	if err := m.Validate(); err == nil {
		t.Error("expected validation to reject zero timestamp")
	}
}
