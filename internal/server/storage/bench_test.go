package storage

import (
	"fmt"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/testutil"
)

// Benchmarks live in files without a build tag on purpose. `go test` compiles
// them but does not run them unless -bench is given, so tagging them buys
// nothing at test time and costs the one thing that matters: a benchmark behind
// a tag is never compiled by CI, and rots into a build failure nobody notices
// until the day they need to measure something.

// sink defeats the compiler. Without a reachable use of the result, the whole
// call is dead code and the loop measures nothing at all.
var (
	sinkString  string
	sinkMetrics []model.Metric
	sinkBool    bool
)

func mustAgg(name string) Aggregator {
	a, err := AggregatorByName(name)
	if err != nil {
		panic(err)
	}
	return a
}

func labelsOfSize(n int) map[string]string {
	labels := make(map[string]string, n)
	for i := 0; i < n; i++ {
		labels[fmt.Sprintf("label_%02d", i)] = fmt.Sprintf("value_%02d", i)
	}
	return labels
}

// BenchmarkSeriesKey is the one that matters: SeriesKey runs once per metric
// written, on every backend, which at a thousand agents reporting a hundred
// series every fifteen seconds is ~6.7k calls/sec through a single function.
// Its allocations are what feed the GC.
func BenchmarkSeriesKey(b *testing.B) {
	for _, n := range []int{0, 1, 3, 8, 16} {
		labels := labelsOfSize(n)
		b.Run(fmt.Sprintf("labels=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sinkString = SeriesKey("http_requests_total", labels)
			}
		})
	}
}

// BenchmarkSeriesKeyEscaped measures the slow path: a label value carrying a
// delimiter must be escaped. Real label values (paths, URLs, error messages)
// contain commas and equals signs often enough that this path is not exotic.
func BenchmarkSeriesKeyEscaped(b *testing.B) {
	labels := map[string]string{
		"host":  "web-1",
		"path":  "/api/v1/query?name=cpu,agg=avg",
		"error": `dial tcp: lookup {host}: no such host`,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkString = SeriesKey("http_requests_total", labels)
	}
}

func BenchmarkParseSeriesKey(b *testing.B) {
	key := SeriesKey("http_requests_total", labelsOfSize(3))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		name, labels, err := ParseSeriesKey(key)
		if err != nil {
			b.Fatal(err)
		}
		sinkString = name
		sinkBool = labels != nil
	}
}

func BenchmarkMemoryStorageWrite(b *testing.B) {
	// Vary the input: writing one hot key over and over measures the CPU cache,
	// not the store. 512 distinct series is a realistic per-agent cardinality.
	metrics := testutil.Metrics("bench", 512)

	b.ReportAllocs()
	b.ResetTimer()
	s := NewMemoryStorage()
	for i := 0; i < b.N; i++ {
		if err := s.Write(metrics[i%len(metrics)]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMemoryStorageWriteBatch(b *testing.B) {
	for _, size := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("batch=%d", size), func(b *testing.B) {
			metrics := testutil.Metrics("bench", size)
			b.ReportAllocs()
			b.ResetTimer()
			s := NewMemoryStorage()
			for i := 0; i < b.N; i++ {
				if err := s.WriteBatch(metrics); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkMemoryStorageQuery shows how a query's cost splits between finding
// the series (the name index) and reducing its points (the aggregator).
func BenchmarkMemoryStorageQuery(b *testing.B) {
	s := NewMemoryStorage()
	base := testutil.BaseTime
	for i := 0; i < 10_000; i++ {
		_ = s.Write(model.Metric{
			Name:      "cpu_usage_percent",
			Type:      model.MetricTypeGauge,
			Value:     float64(i % 100),
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Labels:    map[string]string{"host": fmt.Sprintf("web-%d", i%4)},
		})
	}

	queries := map[string]Query{
		"raw":       {Name: "cpu_usage_percent", From: base, To: base.Add(time.Hour)},
		"filtered":  {Name: "cpu_usage_percent", Labels: map[string]string{"host": "web-1"}},
		"aggregate": {Name: "cpu_usage_percent", Aggregator: mustAgg("avg"), Step: time.Minute, From: base, To: base.Add(2 * time.Hour)},
		"limited":   {Name: "cpu_usage_percent", Limit: 10},
	}

	for name, q := range queries {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := s.Query(q)
				if err != nil {
					b.Fatal(err)
				}
				sinkMetrics = out
			}
		})
	}
}

func BenchmarkApplyQuery(b *testing.B) {
	points := make([]Point, 1000)
	for i := range points {
		points[i] = Point{Timestamp: testutil.BaseTime.Add(time.Duration(i) * time.Second), Value: float64(i)}
	}
	labels := map[string]string{"host": "web-1"}

	for _, agg := range []string{"", "avg", "p95"} {
		name := agg
		q := Query{Name: "cpu", Step: time.Minute}
		if agg == "" {
			name = "raw"
		} else {
			q.Aggregator = mustAgg(agg)
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sinkMetrics = ApplyQuery(q, "cpu", model.MetricTypeGauge, labels, points)
			}
		})
	}
}

func BenchmarkMatchLabels(b *testing.B) {
	labels := labelsOfSize(8)
	filter := map[string]string{"label_00": "value_00", "label_07": "value_07"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkBool = MatchLabels(labels, filter)
	}
}
