package grpcconv

import (
	"strconv"
	"testing"

	"metrics-system/internal/model"
	metricspb "metrics-system/internal/proto/metricspb"
	"metrics-system/internal/testutil"
)

// Sinks stop the compiler from proving the conversions dead.
var (
	sinkPBMetric *metricspb.Metric
	sinkPBBatch  *metricspb.Batch
	sinkMetric   model.Metric
	sinkErr      error
)

func BenchmarkMetricToProto(b *testing.B) {
	m := testutil.NewMetric().
		WithLabel("host", "web-1").
		WithLabel("region", "us-east-1").
		Build()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkPBMetric = MetricToProto(m)
	}
}

func BenchmarkMetricFromProto(b *testing.B) {
	pm := MetricToProto(testutil.NewMetric().
		WithLabel("host", "web-1").
		WithLabel("region", "us-east-1").
		Build())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkMetric, sinkErr = MetricFromProto(pm)
	}
}

// BenchmarkBatchToProto scales the batch so the per-metric conversion and the
// map allocations for labels dominate, not the fixed setup.
func BenchmarkBatchToProto(b *testing.B) {
	for _, size := range []int{1, 10, 100, 1000} {
		batch := model.Batch{AgentID: "bench-agent", Metrics: testutil.Metrics("m", size)}
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sinkPBBatch = BatchToProto(batch)
			}
		})
	}
}
