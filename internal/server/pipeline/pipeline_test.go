package pipeline

import (
	"errors"
	"io"
	"log/slog"
	"math"
	"runtime"
	"sync"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
	"metrics-system/internal/testutil"
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

// mixedBatch carries one valid metric and two that fail validation (whitespace
// name, infinite value), so a drained pipeline splits it 1 Stored / 2 Invalid.
func mixedBatch(seed int) model.Batch {
	return testutil.NewBatch().WithMetrics(
		testutil.NewMetric().WithName("ok").WithValue(float64(seed)).Build(),
		testutil.NewMetric().WithName("bad name").Build(),
		testutil.NewMetric().WithValue(math.Inf(1)).Build(),
	).Build()
}

// TestPipelineNoGoroutineLeakAfterShutdown proves Shutdown joins every stage
// goroutine it started: unpack, the validate/enrich pools, both closers and the
// store workers. A stage that outlives Shutdown is a per-restart leak.
func TestPipelineNoGoroutineLeakAfterShutdown(t *testing.T) {
	defer testutil.NoLeaks(t)()

	p := New(storage.NewMemoryStorage(),
		Config{IngestBuffer: 128, ValidateWorkers: 4, EnrichWorkers: 4, StoreWorkers: 2}, testLogger())
	p.Start()
	for i := 0; i < 300; i++ {
		for !p.Ingest(validBatch(i)) {
			runtime.Gosched()
		}
	}
	p.Shutdown()
}

// TestPipelineConcurrentIngestIsRaceFree hammers Ingest from many goroutines
// while the stages drain; run under -race it is the only way the shared channels
// and atomic counters are exercised for data races. It also re-checks the
// conservation of ingested metrics under contention.
func TestPipelineConcurrentIngestIsRaceFree(t *testing.T) {
	p := New(storage.NewMemoryStorage(),
		Config{IngestBuffer: 256, ValidateWorkers: 4, EnrichWorkers: 4, StoreWorkers: 2}, testLogger())
	p.Start()

	const goroutines, perG = 100, 50
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				p.Ingest(validBatch(g*perG + i)) // drops on backpressure are fine here
			}
		}(g)
	}
	wg.Wait()
	p.Shutdown()

	// Every metric that entered the pipeline ends as Stored or Invalid — memory
	// storage never errors, so nothing is lost mid-flight. (Dropped is a separate
	// population; see TestPipelineDroppedIsDisjointFromIngested.)
	got := p.Stats()
	if got.Ingested != got.Stored+got.Invalid {
		t.Errorf("ingested %d != stored %d + invalid %d", got.Ingested, got.Stored, got.Invalid)
	}
}

// TestPipelineDrainsEveryIngestedMetric is the conservation invariant: after a
// clean Shutdown, Ingested == Stored + Dropped + Invalid. The buffer is sized to
// hold every batch, so nothing is ever rejected at ingress (Dropped stays 0) —
// the regime the invariant is meant for, where no metric that entered the
// pipeline may be lost on drain.
//
// The buffer must NOT be undersized here: Ingest bumps Dropped on every rejected
// attempt, so a retry-until-accepted loop inflates Dropped far past the number of
// metrics actually lost (see TestPipelineRetryInflatesDropped).
func TestPipelineDrainsEveryIngestedMetric(t *testing.T) {
	const batches = 500
	p := New(storage.NewMemoryStorage(),
		Config{IngestBuffer: batches + 1, ValidateWorkers: 4, EnrichWorkers: 4, StoreWorkers: 1}, testLogger())
	p.Start()

	var offered int64
	for i := 0; i < batches; i++ {
		b := mixedBatch(i)
		offered += int64(len(b.Metrics))
		if !p.Ingest(b) {
			t.Fatalf("batch %d rejected, but the buffer holds every batch", i)
		}
	}
	p.Shutdown()

	got := p.Stats()
	if got.Dropped != 0 {
		t.Fatalf("Dropped = %d, want 0 (buffer holds every batch)", got.Dropped)
	}
	if got.Ingested != offered {
		t.Fatalf("Ingested = %d, want %d", got.Ingested, offered)
	}
	if got.Ingested != got.Stored+got.Dropped+got.Invalid {
		t.Fatalf("conservation broken: Ingested=%d != Stored=%d + Dropped=%d + Invalid=%d",
			got.Ingested, got.Stored, got.Dropped, got.Invalid)
	}
	if want := int64(batches); got.Stored != want || got.Invalid != 2*want {
		t.Fatalf("split = %d stored / %d invalid, want %d / %d", got.Stored, got.Invalid, want, 2*want)
	}
}

// The counters are what an operator reads at 3am, so their meaning is pinned
// here. `dropped` is disjoint from `ingested`: a metric refused at the door was
// never ingested, so the conserved total across an Ingest attempt is
// `offered == ingested + dropped`, and the naive `ingested == stored + dropped +
// invalid` is simply false under backpressure.
func TestDroppedIsDisjointFromIngested(t *testing.T) {
	// Not started: nothing drains ingestCh, so the buffer fills and the rest are
	// rejected at the door. No goroutines, no timing, no flake.
	const buf, batches, per = 4, 100, 3
	p := New(storage.NewMemoryStorage(), Config{IngestBuffer: buf}, testLogger())

	accepted := 0
	for i := 0; i < batches; i++ {
		if p.Ingest(model.Batch{AgentID: "t", Metrics: make([]model.Metric, per)}) {
			accepted++
		}
	}

	got := p.Stats()
	if got.Ingested != int64(accepted*per) {
		t.Errorf("Ingested = %d, want %d", got.Ingested, accepted*per)
	}
	if got.Dropped != int64((batches-accepted)*per) {
		t.Errorf("Dropped = %d, want %d", got.Dropped, (batches-accepted)*per)
	}
	if got.Ingested+got.Dropped != int64(batches*per) {
		t.Errorf("Ingested+Dropped = %d, want the full offered total %d", got.Ingested+got.Dropped, batches*per)
	}
}

// failingStorage fails every write, the way a full disk or a revoked mount does.
type failingStorage struct{ storage.Storage }

func (failingStorage) WriteBatch([]model.Metric) error { return errors.New("disk on fire") }

// A metric that passed validation and then failed to be written is lost, and
// unlike a dropped one it was never refused: the caller was told 202. Before the
// `failed` counter existed those metrics left no trace but a log line, and
// `ingested` quietly exceeded `stored + invalid` by an unknown amount.
func TestStorageWriteFailuresAreCounted(t *testing.T) {
	p := New(failingStorage{storage.NewMemoryStorage()},
		Config{IngestBuffer: 64, ValidateWorkers: 1, EnrichWorkers: 1, StoreWorkers: 1}, testLogger())
	p.Start()

	const batches = 10
	var offered int64
	for i := 0; i < batches; i++ {
		b := validBatch(i)
		offered += int64(len(b.Metrics))
		if !p.Ingest(b) {
			t.Fatalf("batch %d rejected by a buffer that holds them all", i)
		}
	}
	p.Shutdown()

	got := p.Stats()
	if got.Stored != 0 {
		t.Errorf("Stored = %d, want 0: every write failed", got.Stored)
	}
	if got.Failed != offered {
		t.Errorf("Failed = %d, want %d: every accepted metric was lost to the storage error", got.Failed, offered)
	}
	if got.Ingested != got.Stored+got.Invalid+got.Failed {
		t.Errorf("conservation broken: Ingested=%d != Stored=%d + Invalid=%d + Failed=%d",
			got.Ingested, got.Stored, got.Invalid, got.Failed)
	}
}
