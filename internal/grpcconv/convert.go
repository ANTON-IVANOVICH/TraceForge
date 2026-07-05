// Package grpcconv converts between the domain model (internal/model) and its
// protobuf twin (internal/proto/metricspb). It is deliberately pure: it maps
// fields and validates enum values, but leaves business validation (required
// name, non-zero timestamp) to model.Validate.
package grpcconv

import (
	"fmt"
	"time"

	"metrics-system/internal/model"
	metricspb "metrics-system/internal/proto/metricspb"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// MetricTypeToProto maps a model metric type to its protobuf enum.
func MetricTypeToProto(t model.MetricType) metricspb.MetricType {
	switch t {
	case model.MetricTypeGauge:
		return metricspb.MetricType_METRIC_TYPE_GAUGE
	case model.MetricTypeCounter:
		return metricspb.MetricType_METRIC_TYPE_COUNTER
	default:
		return metricspb.MetricType_METRIC_TYPE_UNSPECIFIED
	}
}

// MetricTypeFromProto maps a protobuf enum to a model metric type. UNSPECIFIED
// and unknown values are rejected, mirroring the strict JSON path.
func MetricTypeFromProto(t metricspb.MetricType) (model.MetricType, error) {
	switch t {
	case metricspb.MetricType_METRIC_TYPE_GAUGE:
		return model.MetricTypeGauge, nil
	case metricspb.MetricType_METRIC_TYPE_COUNTER:
		return model.MetricTypeCounter, nil
	default:
		return 0, fmt.Errorf("unknown metric type: %v", t)
	}
}

// MetricToProto converts a single metric.
func MetricToProto(m model.Metric) *metricspb.Metric {
	pm := &metricspb.Metric{
		Name:      m.Name,
		Type:      MetricTypeToProto(m.Type),
		Value:     m.Value,
		Timestamp: timestamppb.New(m.Timestamp),
	}
	if len(m.Labels) > 0 {
		pm.Labels = make(map[string]string, len(m.Labels))
		for k, v := range m.Labels {
			pm.Labels[k] = v
		}
	}
	return pm
}

// MetricFromProto converts a single metric. A nil timestamp maps to the zero
// time, which model.Validate then rejects.
func MetricFromProto(pm *metricspb.Metric) (model.Metric, error) {
	if pm == nil {
		return model.Metric{}, fmt.Errorf("nil metric")
	}
	mt, err := MetricTypeFromProto(pm.GetType())
	if err != nil {
		return model.Metric{}, err
	}
	var ts time.Time
	if pm.GetTimestamp() != nil {
		ts = pm.GetTimestamp().AsTime()
	}
	m := model.Metric{
		Name:      pm.GetName(),
		Type:      mt,
		Value:     pm.GetValue(),
		Timestamp: ts,
	}
	if len(pm.GetLabels()) > 0 {
		m.Labels = make(map[string]string, len(pm.GetLabels()))
		for k, v := range pm.GetLabels() {
			m.Labels[k] = v
		}
	}
	return m, nil
}

// BatchToProto converts a whole batch.
func BatchToProto(b model.Batch) *metricspb.Batch {
	pb := &metricspb.Batch{AgentId: b.AgentID}
	if len(b.Metrics) > 0 {
		pb.Metrics = make([]*metricspb.Metric, len(b.Metrics))
		for i := range b.Metrics {
			pb.Metrics[i] = MetricToProto(b.Metrics[i])
		}
	}
	return pb
}

// BatchFromProto converts a whole batch. It fails if any metric carries an
// invalid type; other validation is left to model.Batch.Validate.
func BatchFromProto(pb *metricspb.Batch) (model.Batch, error) {
	if pb == nil {
		return model.Batch{}, fmt.Errorf("nil batch")
	}
	b := model.Batch{AgentID: pb.GetAgentId()}
	if len(pb.GetMetrics()) > 0 {
		b.Metrics = make([]model.Metric, len(pb.GetMetrics()))
		for i, pm := range pb.GetMetrics() {
			m, err := MetricFromProto(pm)
			if err != nil {
				return model.Batch{}, fmt.Errorf("metric[%d]: %w", i, err)
			}
			b.Metrics[i] = m
		}
	}
	return b, nil
}
