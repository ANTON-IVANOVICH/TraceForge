//go:build integration

package bolt

import (
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
	"metrics-system/internal/testutil"
)

// These tests exercise a real bbolt file on disk: the transactional guarantees,
// the on-disk file lock, and concurrent access — none of which the in-memory
// storage tests (or bolt_test.go's single-threaded reopen) can observe.

// TestBolt_DataAndAggregationSurviveReopen writes a realistic multi-series set,
// closes the handle (releasing the file lock and flushing bbolt's pages), then
// reopens a fresh handle on the same file and re-runs an aggregation. It proves
// aggregate reads — not just raw point counts — survive a process restart.
func TestBolt_DataAndAggregationSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "series.bolt")
	base := testutil.BaseTime

	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	batch := testutil.NewBatch().
		WithSeries("load", 300, time.Second, func(i int) float64 { return float64(i) }).
		Build()
	if err := s.WriteBatch(batch.Metrics); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := New(path)
	if err != nil {
		t.Fatalf("reopen after clean close: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	if raw, err := s2.Query(storage.Query{Name: "load"}); err != nil || len(raw) != 300 {
		t.Fatalf("raw reopen query: got %d points err=%v, want 300", len(raw), err)
	}

	sum, err := storage.AggregatorByName("sum")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Query(storage.Query{
		Name: "load", Aggregator: sum, From: base, To: base.Add(time.Hour), Step: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	// sum(0..299) = 299*300/2 = 44850.
	if len(got) != 1 || got[0].Value != 44850 {
		t.Fatalf("aggregation after reopen = %+v, want single 44850", got)
	}
}

// TestBolt_SecondOpenFailsFastOnLock proves the 1s open Timeout: a second New on
// a held file must return an error quickly instead of blocking forever, and the
// lock must be released on Close so a later open succeeds.
func TestBolt_SecondOpenFailsFastOnLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.bolt")

	s1, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	s2, err := New(path)
	elapsed := time.Since(start)
	if err == nil {
		_ = s2.Close()
		t.Fatal("second open of a locked file should fail")
	}
	// The bbolt Timeout is 1s; allow generous slack for a loaded CI box, but a
	// hang (no timeout) would blow past this and is the failure we guard against.
	if elapsed > 5*time.Second {
		t.Fatalf("second open took %s; it must fail fast on the timeout, not hang", elapsed)
	}

	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s3, err := New(path)
	if err != nil {
		t.Fatalf("open after the lock was released: %v", err)
	}
	_ = s3.Close()
}

// TestBolt_ConcurrentWriteAndQueryRaceFree runs many writers (each owning a
// distinct series) alongside readers against one open database. Under -race it
// is the only check that bbolt's writer serialization and concurrent View
// transactions are used correctly from Go.
func TestBolt_ConcurrentWriteAndQueryRaceFree(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "concurrent.bolt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	base := testutil.BaseTime
	const writers, perWriter = 8, 200

	var wg sync.WaitGroup
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				m := testutil.NewMetric().
					WithName("cpu").
					WithLabel("w", strconv.Itoa(g)).
					WithValue(float64(i)).
					WithTimestamp(base.Add(time.Duration(g*perWriter+i) * time.Millisecond)).
					Build()
				if err := s.WriteBatch([]model.Metric{m}); err != nil {
					t.Errorf("writer %d: %v", g, err)
					return
				}
			}
		}(g)
	}

	// Readers run for the lifetime of the writers, hammering the View path.
	var queryErr atomic.Bool
	stop := make(chan struct{})
	var readWg sync.WaitGroup
	for r := 0; r < 3; r++ {
		readWg.Add(1)
		go func() {
			defer readWg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := s.Query(storage.Query{Name: "cpu"}); err != nil {
						queryErr.Store(true)
						return
					}
				}
			}
		}()
	}

	wg.Wait()
	close(stop)
	readWg.Wait()

	if queryErr.Load() {
		t.Fatal("a concurrent Query returned an error")
	}
	if st := s.Stats(); st.Series != writers || st.Points != writers*perWriter {
		t.Fatalf("stats = %+v, want series=%d points=%d", st, writers, writers*perWriter)
	}
	// One writer's series must be readable in full and in isolation.
	got, err := s.Query(storage.Query{Name: "cpu", Labels: map[string]string{"w": "3"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != perWriter {
		t.Fatalf("series w=3 has %d points, want %d", len(got), perWriter)
	}
}

// TestBolt_LargeBatchSingleTxn commits 10k points in a single WriteBatch (one
// bbolt transaction) and reads them back with aggregation — verifying the whole
// batch is atomically durable and correctly indexed.
func TestBolt_LargeBatchSingleTxn(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "large.bolt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	base := testutil.BaseTime
	const n = 10000
	ms := make([]model.Metric, n)
	for i := range ms {
		ms[i] = testutil.NewMetric().
			WithName("big").
			WithValue(float64(i)).
			WithTimestamp(base.Add(time.Duration(i) * time.Millisecond)).
			Build()
	}
	if err := s.WriteBatch(ms); err != nil {
		t.Fatalf("commit 10k in one txn: %v", err)
	}

	if raw, err := s.Query(storage.Query{Name: "big"}); err != nil || len(raw) != n {
		t.Fatalf("raw readback: got %d err=%v, want %d", len(raw), err, n)
	}

	sum, _ := storage.AggregatorByName("sum")
	got, err := s.Query(storage.Query{
		Name: "big", Aggregator: sum, From: base, To: base.Add(time.Hour), Step: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	// sum(0..9999) = 9999*10000/2 = 49995000.
	if len(got) != 1 || got[0].Value != 49995000 {
		t.Fatalf("sum over 10k = %+v, want single 49995000", got)
	}
	if st := s.Stats(); st.Points != n {
		t.Errorf("stats points = %d, want %d", st.Points, n)
	}
}

// TestBolt_SeriesKeyInjectiveThroughDisk proves the escaped SeriesKey stays
// injective through a real bbolt file: series whose label sets collide under the
// naive name{k=v,...} encoding must round-trip as distinct series, each with its
// own points and its exact label set, after a close/reopen cycle.
func TestBolt_SeriesKeyInjectiveThroughDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inject.bolt")
	base := testutil.BaseTime

	type spec struct {
		labels map[string]string
		val    float64
	}
	// Each adjacent pair collides under the delimiter-naive encoding; the special
	// bytes , = { } and \ all appear in a value at least once.
	series := []spec{
		{map[string]string{"a": "b,c=d"}, 1},       // naive: cpu{a=b,c=d}
		{map[string]string{"a": "b", "c": "d"}, 2}, // naive: cpu{a=b,c=d} — collides with above
		{map[string]string{"k": "v,x="}, 3},        // naive: cpu{k=v,x=}
		{map[string]string{"k": "v", "x": ""}, 4},  // naive: cpu{k=v,x=} — collides with above
		{map[string]string{"path": `a\b{c}d`}, 5},  // backslash + braces
		{map[string]string{"tag": "closed}"}, 6},   // stray close brace
	}

	wantByKey := make(map[string]spec, len(series))
	for _, s := range series {
		wantByKey[storage.SeriesKey("cpu", s.labels)] = s
	}
	// Sanity: distinct specs must produce distinct keys — that is the property.
	if len(wantByKey) != len(series) {
		t.Fatalf("SeriesKey collapsed %d specs to %d keys — not injective", len(series), len(wantByKey))
	}

	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	ms := make([]model.Metric, 0, len(series))
	for i, s := range series {
		ms = append(ms, model.Metric{
			Name:      "cpu",
			Type:      model.MetricTypeGauge,
			Value:     s.val,
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Labels:    s.labels,
		})
	}
	if err := db.WriteBatch(ms); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	got, err := db2.Query(storage.Query{Name: "cpu"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(series) {
		t.Fatalf("got %d metrics after reopen, want %d (series merged on disk)", len(got), len(series))
	}

	seen := make(map[string]bool, len(series))
	for _, m := range got {
		key := storage.SeriesKey(m.Name, m.Labels)
		want, ok := wantByKey[key]
		if !ok {
			t.Errorf("unexpected/merged series with labels %v (key %q)", m.Labels, key)
			continue
		}
		if seen[key] {
			t.Errorf("series %q returned twice", key)
		}
		seen[key] = true
		if m.Value != want.val {
			t.Errorf("series %q: value %v, want %v (points landed in the wrong series)", key, m.Value, want.val)
		}
		testutil.AssertLabelsEqual(t, want.labels, m.Labels)
	}
	if len(seen) != len(series) {
		t.Fatalf("recovered %d distinct series, want %d", len(seen), len(series))
	}
}
