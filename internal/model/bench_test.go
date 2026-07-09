package model_test

import (
	"encoding/json"
	"strconv"
	"testing"

	"metrics-system/internal/model"
	"metrics-system/internal/testutil"
)

// Package-level sinks defeat the dead-code eliminator: without them the compiler
// can prove the benchmarked results are unused and delete the work being timed.
var (
	sinkErr    error
	sinkBytes  []byte
	sinkMetric model.Metric
)

func BenchmarkMetricMarshalJSON(b *testing.B) {
	m := testutil.NewMetric().
		WithLabel("host", "web-1").
		WithLabel("region", "us-east-1").
		Build()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkBytes, sinkErr = json.Marshal(m)
	}
}

func BenchmarkMetricUnmarshalJSON(b *testing.B) {
	m := testutil.NewMetric().
		WithLabel("host", "web-1").
		WithLabel("region", "us-east-1").
		Build()
	data, err := json.Marshal(m)
	if err != nil {
		b.Fatalf("seed marshal: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var out model.Metric
		sinkErr = json.Unmarshal(data, &out)
		sinkMetric = out
	}
}

func BenchmarkMetricValidate(b *testing.B) {
	m := testutil.NewMetric().Build()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkErr = m.Validate()
	}
}

// BenchmarkBatchValidate scales the batch to expose the per-metric cost and any
// allocation that grows with size (Validate should allocate nothing).
func BenchmarkBatchValidate(b *testing.B) {
	for _, size := range []int{1, 10, 100, 1000} {
		batch := model.Batch{AgentID: "bench-agent", Metrics: testutil.Metrics("m", size)}
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sinkErr = batch.Validate()
			}
		})
	}
}
