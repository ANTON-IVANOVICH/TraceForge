package pipeline

import (
	"fmt"
	"math"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
	"metrics-system/internal/testutil"
)

// Package-level sinks keep the compiler from deleting the calls these loops
// measure.
var (
	sinkBool bool
	sinkErr  error
)

// benchBatch builds a batch of n metrics across 16 distinct series names — a
// realistic working set rather than one hot key. Labels are left nil on purpose:
// unpackStage allocates a fresh label map for its copy of each metric, so reusing
// one batch across iterations mutates nothing shared and the pipeline stays
// race-free without a per-Ingest copy.
func benchBatch(n int) model.Batch {
	ms := make([]model.Metric, n)
	for i := range ms {
		ms[i] = model.Metric{
			Name:      fmt.Sprintf("cpu_%d", i%16),
			Type:      model.MetricTypeGauge,
			Value:     float64(i),
			Timestamp: testutil.BaseTime.Add(time.Duration(i) * time.Second),
		}
	}
	return model.Batch{AgentID: "bench", Metrics: ms}
}

// workerCounts is the {1, 2, 4, NumCPU} matrix with duplicates removed.
func workerCounts() []int {
	seen := map[int]bool{}
	var out []int
	for _, n := range []int{1, 2, 4, runtime.NumCPU()} {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// countingStore wraps a Storage to count WriteBatch calls, so a benchmark can
// report how many metrics each physical write carries.
type countingStore struct {
	storage.Storage
	calls  atomic.Int64
	points atomic.Int64
}

func (c *countingStore) WriteBatch(ms []model.Metric) error {
	c.calls.Add(1)
	c.points.Add(int64(len(ms)))
	return c.Storage.WriteBatch(ms)
}

// BenchmarkPipelineThroughput answers the question the worker knobs exist to
// answer: does the pipeline actually scale with validate/enrich parallelism?
// Timing spans ingest through a full Shutdown drain, so metrics/s is end-to-end,
// not just enqueue cost. If the number is flat across the worker matrix, the
// parallelism is buying nothing and the store stage (StoreWorkers: 1, serialized
// behind the storage lock) is the ceiling.
func BenchmarkPipelineThroughput(b *testing.B) {
	const batchSize = 50
	batch := benchBatch(batchSize)

	for _, workers := range workerCounts() {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			p := New(storage.NewMemoryStorage(), Config{
				IngestBuffer:    256,
				ValidateWorkers: workers,
				EnrichWorkers:   workers,
				StoreWorkers:    1,
			}, testLogger())
			p.Start()

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for !p.Ingest(batch) {
					runtime.Gosched()
				}
			}
			p.Shutdown() // timed: the drain is part of end-to-end throughput
			b.StopTimer()

			b.ReportMetric(float64(b.N)*batchSize/b.Elapsed().Seconds(), "metrics/s")
		})
	}
}

// BenchmarkIngestBackpressure separates the two arms of Ingest's select: the
// fast path (buffer has room, successful send + atomic bump) and the drop path
// (buffer full, default arm + atomic bump). The delta is what the HTTP handler
// pays per request in each regime.
func BenchmarkIngestBackpressure(b *testing.B) {
	batch := benchBatch(1)

	b.Run("fast_path", func(b *testing.B) {
		// Started, generous buffer, enough workers: the store keeps up, so Ingest
		// reliably finds room and takes the success arm.
		p := New(storage.NewMemoryStorage(),
			Config{IngestBuffer: 1 << 16, ValidateWorkers: 4, EnrichWorkers: 4, StoreWorkers: 2}, testLogger())
		p.Start()
		defer p.Shutdown()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkBool = p.Ingest(batch)
		}
	})

	b.Run("drop_path", func(b *testing.B) {
		// Never started and the single buffer slot pre-filled: every Ingest hits
		// the default arm and records a drop.
		p := New(storage.NewMemoryStorage(), Config{IngestBuffer: 1}, testLogger())
		p.Ingest(batch)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkBool = p.Ingest(batch)
		}
	})
}

// BenchmarkValidate profiles the per-metric gate in isolation. The sub-cases are
// ordered by how early validate can bail, so the spread shows where the cost is:
// the valid case runs the full ContainsAny scan, the reject cases short-circuit.
func BenchmarkValidate(b *testing.B) {
	cases := []struct {
		name string
		m    model.Metric
	}{
		{"valid", model.Metric{Name: "http_requests_total", Type: model.MetricTypeCounter, Value: 42}},
		{"empty_name", model.Metric{Name: "", Type: model.MetricTypeGauge, Value: 1}},
		{"whitespace_name", model.Metric{Name: "bad name here", Type: model.MetricTypeGauge, Value: 1}},
		{"bad_type", model.Metric{Name: "http_requests_total", Type: model.MetricType(9), Value: 1}},
		{"nan_value", model.Metric{Name: "http_requests_total", Type: model.MetricTypeGauge, Value: math.NaN()}},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sinkErr = validate(c.m)
			}
		})
	}
}

// BenchmarkStoreStageBatching shows the storeStage collapsing many metrics into
// few WriteBatch calls. The reported metrics/WriteBatch >> 1 is the whole point
// of the stage: on a transactional backend each WriteBatch is one commit, so the
// batching factor is the write-amplification saved.
func BenchmarkStoreStageBatching(b *testing.B) {
	const batchSize = 100
	batch := benchBatch(batchSize)

	cs := &countingStore{Storage: storage.NewMemoryStorage()}
	p := New(cs, Config{IngestBuffer: 1024, ValidateWorkers: 4, EnrichWorkers: 4, StoreWorkers: 1}, testLogger())
	p.Start()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for !p.Ingest(batch) {
			runtime.Gosched()
		}
	}
	p.Shutdown()
	b.StopTimer()

	if calls := cs.calls.Load(); calls > 0 {
		b.ReportMetric(float64(cs.points.Load())/float64(calls), "metrics/WriteBatch")
	}
	b.ReportMetric(float64(b.N)*batchSize/b.Elapsed().Seconds(), "metrics/s")
}
