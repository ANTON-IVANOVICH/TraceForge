//go:build integration

package pipeline

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
	"metrics-system/internal/server/storage/bolt"
	"metrics-system/internal/server/storage/tsdb"
	"metrics-system/internal/testutil"
)

// These run the whole pipeline against a real on-disk backend and then reopen
// that backend from scratch — the durability boundary the memory-backed unit
// tests cannot see. testLogger is shared with pipeline_test.go.

// diskBackend is a persistent storage backend that can be reopened from the same
// directory, so a test can assert what actually survived to disk.
type diskBackend struct {
	name string
	open func(t *testing.T, dir string) storage.Storage
}

func diskBackends() []diskBackend {
	return []diskBackend{
		{"bolt", func(t *testing.T, dir string) storage.Storage {
			s, err := bolt.New(filepath.Join(dir, "db.bolt"))
			if err != nil {
				t.Fatalf("open bolt: %v", err)
			}
			return s
		}},
		{"tsdb", func(t *testing.T, dir string) storage.Storage {
			s, err := tsdb.Open(dir, testLogger())
			if err != nil {
				t.Fatalf("open tsdb: %v", err)
			}
			return s
		}},
	}
}

// TestPipeline_DurableToDiskAcrossReopen ingests 10k metrics end-to-end, drains
// with Shutdown, then reopens the backend from disk and counts — proving every
// stored metric is durable across a restart, not merely present in memory.
func TestPipeline_DurableToDiskAcrossReopen(t *testing.T) {
	const n = 10000
	metrics := testutil.Metrics("m", n)

	for _, be := range diskBackends() {
		t.Run(be.name, func(t *testing.T) {
			dir := t.TempDir()
			store := be.open(t, dir)

			pipe := New(store, Config{IngestBuffer: 1000, ValidateWorkers: 4, EnrichWorkers: 4, StoreWorkers: 1}, testLogger())
			pipe.Start()

			for i := 0; i < n; i += 500 {
				end := min(i+500, n)
				b := testutil.NewBatch().WithMetrics(metrics[i:end]...).Build()
				for !pipe.Ingest(b) { // never leave anything un-ingested
					runtime.Gosched()
				}
			}
			pipe.Shutdown()

			if snap := pipe.Stats(); snap.Stored != n || snap.Dropped != 0 || snap.Invalid != 0 {
				t.Fatalf("pipeline stats = %+v, want stored=%d, no drops/invalid", snap, n)
			}

			// Release the file and reopen a fresh handle: this reads from disk, not
			// from the still-warm process state.
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := be.open(t, dir)
			t.Cleanup(func() { _ = reopened.Close() })

			if st := reopened.Stats(); st.Points != n {
				t.Fatalf("durable points after reopen = %d, want %d", st.Points, n)
			}
		})
	}
}

// blockingStore stalls every WriteBatch until release is closed, so the pipeline
// cannot drain and backpressure is forced deterministically.
type blockingStore struct {
	storage.Storage
	release chan struct{}
}

func (b *blockingStore) WriteBatch(ms []model.Metric) error {
	<-b.release
	return b.Storage.WriteBatch(ms)
}

// TestPipeline_BackpressureDropsAreNeverStored fills the buffers behind a stalled
// store, confirms Ingest rejects and Dropped climbs, then drains and proves the
// accounting is watertight: no metric is both dropped and stored, and the number
// of points on disk equals exactly Stored.
func TestPipeline_BackpressureDropsAreNeverStored(t *testing.T) {
	dir := t.TempDir()
	real, err := bolt.New(filepath.Join(dir, "db.bolt"))
	if err != nil {
		t.Fatal(err)
	}
	bs := &blockingStore{Storage: real, release: make(chan struct{})}

	pipe := New(bs, Config{IngestBuffer: 1, ValidateWorkers: 1, EnrichWorkers: 1, StoreWorkers: 1}, testLogger())
	pipe.Start()

	base := testutil.BaseTime
	const total = 500
	for i := 0; i < total; i++ {
		m := testutil.NewMetric().
			WithName("bp").
			WithValue(float64(i)).
			WithTimestamp(base.Add(time.Duration(i) * time.Millisecond)).
			Build()
		pipe.Ingest(testutil.NewBatch().WithMetrics(m).Build()) // drops are the point
	}

	testutil.Eventually(t, 2*time.Second, 5*time.Millisecond, func() bool {
		return pipe.Stats().Dropped > 0
	}, "no drops observed while the store was blocked")

	close(bs.release) // unblock and let the pipeline drain
	pipe.Shutdown()

	snap := pipe.Stats()
	if snap.Dropped == 0 {
		t.Fatal("expected drops under backpressure")
	}
	if snap.Invalid != 0 {
		t.Errorf("invalid = %d, want 0", snap.Invalid)
	}
	if snap.Stored != snap.Ingested {
		t.Errorf("stored %d != ingested %d — a metric was lost mid-flight", snap.Stored, snap.Ingested)
	}
	if snap.Ingested+snap.Dropped != total {
		t.Errorf("ingested %d + dropped %d != %d offered", snap.Ingested, snap.Dropped, total)
	}

	// The disk is the arbiter: dropped metrics never entered the pipeline, so the
	// durable point count must equal Stored exactly.
	if err := real.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := bolt.New(filepath.Join(dir, "db.bolt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if st := reopened.Stats(); st.Points != snap.Stored {
		t.Fatalf("durable points = %d, want Stored = %d (a dropped metric leaked to disk?)", st.Points, snap.Stored)
	}
}
