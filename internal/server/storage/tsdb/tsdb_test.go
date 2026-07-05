package tsdb

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func gauge(name string, v float64, ts time.Time) model.Metric {
	return model.Metric{Name: name, Type: model.MetricTypeGauge, Value: v, Timestamp: ts}
}

// Data written and then reopened must be recovered from the WAL (no flush).
func TestTSDB_RecoveryFromWAL(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().UTC()

	db, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if err := db.Write(gauge("test", float64(i), base.Add(time.Duration(i)*time.Millisecond))); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	got, err := db2.Query(storage.Query{Name: "test", From: base.Add(-time.Hour), To: base.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 100 {
		t.Fatalf("recovered %d points from WAL, want 100", len(got))
	}
	if got[0].Value != 0 || got[99].Value != 99 {
		t.Errorf("recovered values wrong: first=%v last=%v", got[0].Value, got[99].Value)
	}
}

// After a flush the data lives in a chunk (head empty), and querying still
// returns it; new writes go to a fresh head and merge with the chunk.
func TestTSDB_FlushThenQueryAndMerge(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().UTC()
	db, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for i := 0; i < 10; i++ {
		if err := db.Write(gauge("m", float64(i), base.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.flush(); err != nil {
		t.Fatal(err)
	}
	if !db.head.isEmpty() {
		t.Fatal("head should be empty after flush")
	}

	got, err := db.Query(storage.Query{Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("after flush query got %d, want 10", len(got))
	}

	// New write goes to the fresh head; query must merge chunk + head.
	if err := db.Write(gauge("m", 100, base.Add(20*time.Second))); err != nil {
		t.Fatal(err)
	}
	got2, err := db.Query(storage.Query{Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 11 {
		t.Fatalf("chunk+head merge got %d, want 11", len(got2))
	}
	if got2[10].Value != 100 {
		t.Errorf("merged tail value = %v, want 100", got2[10].Value)
	}
}

// Flush then reopen: chunk data + WAL-only data both survive.
func TestTSDB_PersistAcrossFlushAndReopen(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().UTC()

	db, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_ = db.Write(gauge("m", float64(i), base.Add(time.Duration(i)*time.Second)))
	}
	if err := db.flush(); err != nil { // 5 points -> chunk
		t.Fatal(err)
	}
	_ = db.Write(gauge("m", 99, base.Add(10*time.Second))) // 1 point -> head/WAL
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	got, err := db2.Query(storage.Query{Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("after flush+reopen got %d points, want 6 (5 chunk + 1 wal)", len(got))
	}
	if st := db2.Stats(); st.Points != 6 {
		t.Errorf("stats points = %d, want 6", st.Points)
	}
}

func TestTSDB_Aggregation(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	db, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for i, v := range []float64{10, 20, 30, 40} {
		_ = db.Write(gauge("m", v, base.Add(time.Duration(i)*time.Second)))
	}
	agg, err := storage.AggregatorByName("avg")
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.Query(storage.Query{Name: "m", Aggregator: agg, From: base, To: base.Add(time.Hour), Step: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Value != 25 {
		t.Fatalf("avg got %+v, want single 25", got)
	}
}

func TestTSDB_LockPreventsSecondOpen(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := Open(dir, testLogger()); err == nil {
		t.Fatal("second Open of a locked dir should fail")
	}
}
