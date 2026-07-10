package tsdb

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
	"metrics-system/internal/testutil"
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
	defer func() { _ = db2.Close() }()

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
	defer func() { _ = db.Close() }()

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
	defer func() { _ = db2.Close() }()

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
	defer func() { _ = db.Close() }()

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

// Open starts two background loops (sync + flush); Close cancels their context
// and waits on the WaitGroup. The leak detector proves they actually stop — a
// missed wg.Done or a loop that ignored ctx.Done would leave a goroutine running
// past the test and show up here. NoLeaks is installed first, before Open, so
// its snapshot predates the loops.
func TestTSDB_CloseStopsBackgroundLoops(t *testing.T) {
	defer testutil.NoLeaks(t)()

	dir := t.TempDir()
	db, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	for i := 0; i < 50; i++ {
		if err := db.Write(gauge("m", float64(i), base.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTSDB_LockPreventsSecondOpen(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := Open(dir, testLogger()); err == nil {
		t.Fatal("second Open of a locked dir should fail")
	}
}

// The state this test protects against is the worst one a durable store can be
// in: writes return nil, queries return the data, and the fsync that would make
// any of it survive a power cut has been failing since the disk filled up. The
// write path never learns of it — only the background sync loop does, and before
// Ping existed it logged the error and dropped it on the floor.
//
// Closing the WAL's file underneath the sync loop reproduces the symptom exactly:
// Flush finds an empty buffer and succeeds, then fsync fails on a closed
// descriptor. The database keeps serving.
func TestTSDB_PingReportsAFailingFsync(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	// Close() would fsync a WAL we are about to break; the file lock is released
	// with the temp dir.
	defer func() { _ = releaseLock(db.lock) }()

	if err := db.Ping(t.Context()); err != nil {
		t.Fatalf("Ping on a healthy database: %v", err)
	}

	// Break durability without breaking anything a caller can see.
	if err := db.wal.Close(); err != nil {
		t.Fatalf("closing the wal file: %v", err)
	}

	// syncLoop ticks every syncInterval; give it a few ticks to notice.
	deadline := time.Now().Add(2 * time.Second)
	var pingErr error
	for time.Now().Before(deadline) {
		if pingErr = db.Ping(t.Context()); pingErr != nil {
			break
		}
		time.Sleep(syncInterval / 2)
	}
	if pingErr == nil {
		t.Fatal("Ping still reports the database healthy after every fsync failed")
	}
	if !strings.Contains(pingErr.Error(), "wal not syncing") {
		t.Errorf("Ping error = %v, want it to name the failing fsync", pingErr)
	}

	// The query path is deliberately unaffected: this is the point. A store whose
	// fsync fails is not a store that stops answering, which is why the readiness
	// probe is the only thing that can catch it.
	if _, err := db.Query(storage.Query{Name: "nothing"}); err != nil {
		t.Errorf("Query broke too, so the probe was not the only signal: %v", err)
	}
}

// A disk that filled up and was cleaned out must bring the replica back without
// a restart, so a success has to clear a recorded failure.
func TestTSDB_PingRecoversWhenFsyncStartsWorking(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	db.recordSync(errors.New("no space left on device"))
	if db.Ping(t.Context()) == nil {
		t.Fatal("Ping ignores a recorded fsync failure")
	}

	db.recordSync(nil)
	if err := db.Ping(t.Context()); err != nil {
		t.Errorf("Ping still failing after a successful fsync: %v", err)
	}
}

// Ping answers the readiness probe every few seconds. It must not fsync (that
// would be write amplification driven by a health check) and it must not block
// behind a writer.
func TestTSDB_PingDoesNotWaitForWriters(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	db.mu.Lock()
	defer db.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- db.Ping(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Ping: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ping blocked behind a writer holding the database lock")
	}
}
