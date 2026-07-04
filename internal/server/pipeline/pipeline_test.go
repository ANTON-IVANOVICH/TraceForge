package pipeline

import (
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func validBatch(v int) model.Batch {
	return model.Batch{
		AgentID: "test",
		Metrics: []model.Metric{{
			Name:      "test_metric",
			Type:      model.MetricTypeGauge,
			Value:     float64(v),
			Timestamp: time.Now(),
		}},
	}
}

func TestPipeline_HappyPath(t *testing.T) {
	store := storage.NewMemoryStorage()
	p := New(store, Config{IngestBuffer: 1000, ValidateWorkers: 2, EnrichWorkers: 2, StoreWorkers: 1}, testLogger())
	p.Start()

	const n = 100
	for i := 0; i < n; i++ {
		if !p.Ingest(validBatch(i)) {
			t.Fatalf("ingest %d unexpectedly dropped", i)
		}
	}
	p.Shutdown() // drains all in-flight metrics before returning

	got := p.Stats()
	if got.Ingested != n {
		t.Errorf("ingested = %d, want %d", got.Ingested, n)
	}
	if got.Stored != n {
		t.Errorf("stored = %d, want %d (data lost on drain)", got.Stored, n)
	}
	if got.Dropped != 0 || got.Invalid != 0 {
		t.Errorf("dropped=%d invalid=%d, want 0/0", got.Dropped, got.Invalid)
	}
	if st := store.Stats(); st.Points != n {
		t.Errorf("storage points = %d, want %d", st.Points, n)
	}
}

func TestPipeline_Backpressure(t *testing.T) {
	// Not started => nothing drains ingestCh, so a buffer of 2 fills after 2 sends
	// and the 3rd Ingest must be rejected.
	p := New(storage.NewMemoryStorage(), Config{IngestBuffer: 2}, testLogger())

	if !p.Ingest(validBatch(1)) {
		t.Fatal("1st ingest should succeed")
	}
	if !p.Ingest(validBatch(2)) {
		t.Fatal("2nd ingest should succeed")
	}
	if p.Ingest(validBatch(3)) {
		t.Fatal("3rd ingest should be rejected (buffer full = backpressure)")
	}

	got := p.Stats()
	if got.Ingested != 2 {
		t.Errorf("ingested = %d, want 2", got.Ingested)
	}
	if got.Dropped != 1 {
		t.Errorf("dropped = %d, want 1", got.Dropped)
	}
}

func TestPipeline_InvalidDropped(t *testing.T) {
	store := storage.NewMemoryStorage()
	p := New(store, Config{IngestBuffer: 100, ValidateWorkers: 2, EnrichWorkers: 2, StoreWorkers: 1}, testLogger())
	p.Start()

	batch := model.Batch{
		AgentID: "test",
		Metrics: []model.Metric{
			{Name: "ok", Type: model.MetricTypeGauge, Value: 1, Timestamp: time.Now()},
			{Name: "bad name", Type: model.MetricTypeGauge, Value: 2, Timestamp: time.Now()},        // whitespace in name
			{Name: "naninf", Type: model.MetricTypeGauge, Value: math.NaN(), Timestamp: time.Now()}, // NaN value
		},
	}
	if !p.Ingest(batch) {
		t.Fatal("ingest dropped unexpectedly")
	}
	p.Shutdown()

	got := p.Stats()
	if got.Ingested != 3 {
		t.Errorf("ingested = %d, want 3", got.Ingested)
	}
	if got.Invalid != 2 {
		t.Errorf("invalid = %d, want 2", got.Invalid)
	}
	if got.Stored != 1 {
		t.Errorf("stored = %d, want 1", got.Stored)
	}
}

func BenchmarkPipeline_Throughput(b *testing.B) {
	p := New(storage.NewMemoryStorage(), Config{IngestBuffer: 10000, ValidateWorkers: 4, EnrichWorkers: 4, StoreWorkers: 1}, testLogger())
	p.Start()
	defer p.Shutdown()

	batch := validBatch(1)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p.Ingest(batch)
		}
	})
}
