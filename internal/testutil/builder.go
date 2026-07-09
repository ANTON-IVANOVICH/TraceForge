package testutil

import (
	"fmt"
	"time"

	"metrics-system/internal/model"
)

// BaseTime is the fixed instant every builder starts from. Tests that need
// deterministic timestamps (golden files, chunk layouts, fingerprints) must not
// call time.Now(), or they fail on a leap second and pass on a rerun.
var BaseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// MetricBuilder constructs a model.Metric through chained setters. The point is
// insulation: when model.Metric grows a field, this builder changes and the two
// hundred tests that call it do not.
type MetricBuilder struct {
	m model.Metric
}

// NewMetric returns a builder for a valid gauge. Every field already holds a
// sensible value, so a test states only what it actually cares about — the rest
// is noise that would obscure the assertion.
func NewMetric() *MetricBuilder {
	return &MetricBuilder{m: model.Metric{
		Name:      "test_metric",
		Type:      model.MetricTypeGauge,
		Value:     1,
		Timestamp: BaseTime,
		Labels:    map[string]string{},
	}}
}

func (b *MetricBuilder) WithName(n string) *MetricBuilder           { b.m.Name = n; return b }
func (b *MetricBuilder) WithType(t model.MetricType) *MetricBuilder { b.m.Type = t; return b }
func (b *MetricBuilder) WithValue(v float64) *MetricBuilder         { b.m.Value = v; return b }
func (b *MetricBuilder) WithTimestamp(t time.Time) *MetricBuilder   { b.m.Timestamp = t; return b }
func (b *MetricBuilder) WithLabel(k, v string) *MetricBuilder       { b.m.Labels[k] = v; return b }

// WithLabels merges in a whole label set.
func (b *MetricBuilder) WithLabels(labels map[string]string) *MetricBuilder {
	for k, v := range labels {
		b.m.Labels[k] = v
	}
	return b
}

// Build returns the metric with its labels copied, so two Build calls on the
// same builder cannot alias one map — a subtle way for one test case to
// contaminate the next.
func (b *MetricBuilder) Build() model.Metric {
	m := b.m
	if len(b.m.Labels) == 0 {
		m.Labels = nil
		return m
	}
	m.Labels = make(map[string]string, len(b.m.Labels))
	for k, v := range b.m.Labels {
		m.Labels[k] = v
	}
	return m
}

// BatchBuilder constructs a model.Batch.
type BatchBuilder struct {
	b model.Batch
}

// NewBatch returns a builder for a batch from agent "test-agent".
func NewBatch() *BatchBuilder {
	return &BatchBuilder{b: model.Batch{AgentID: "test-agent"}}
}

func (b *BatchBuilder) WithAgentID(id string) *BatchBuilder { b.b.AgentID = id; return b }
func (b *BatchBuilder) WithTenant(t string) *BatchBuilder   { b.b.Tenant = t; return b }

// WithMetrics appends metrics to the batch.
func (b *BatchBuilder) WithMetrics(ms ...model.Metric) *BatchBuilder {
	b.b.Metrics = append(b.b.Metrics, ms...)
	return b
}

// WithSeries appends n points of one series, spaced by step starting at
// BaseTime, with values produced by valueAt. Use it to build the input of a
// range query or a rate() evaluation without a loop in every test.
func (b *BatchBuilder) WithSeries(name string, n int, step time.Duration, valueAt func(i int) float64) *BatchBuilder {
	for i := 0; i < n; i++ {
		b.b.Metrics = append(b.b.Metrics, NewMetric().
			WithName(name).
			WithTimestamp(BaseTime.Add(time.Duration(i)*step)).
			WithValue(valueAt(i)).
			Build())
	}
	return b
}

func (b *BatchBuilder) Build() model.Batch { return b.b }

// Metrics generates n distinct series named "<prefix>_<i>", each with one point.
// Benchmarks use it to build a realistic working set instead of hammering one
// hot key, which would measure the CPU cache rather than the code.
func Metrics(prefix string, n int) []model.Metric {
	out := make([]model.Metric, n)
	for i := range out {
		out[i] = NewMetric().
			WithName(fmt.Sprintf("%s_%d", prefix, i%16)).
			WithValue(float64(i)).
			WithTimestamp(BaseTime.Add(time.Duration(i)*time.Second)).
			WithLabel("host", fmt.Sprintf("web-%d", i%8)).
			WithLabel("region", "us-east-1").
			Build()
	}
	return out
}
