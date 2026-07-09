//go:build integration

package tsdb

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"metrics-system/internal/server/storage"
	"metrics-system/internal/testutil"
)

// These tests drive real WAL and chunk files on disk through failure modes the
// unit tests never reach: a process that dies without a graceful Close, a log
// truncated mid-record, a corrupted record payload, and a query that must merge
// a memory-mapped chunk with a WAL-replayed head. gauge and testLogger are
// shared with tsdb_test.go (same package).

// walHeaderSize mirrors wal.headerSize: length(4) + crc(4) + type(1).
const walHeaderSize = 9

func walFile(dir string) string { return filepath.Join(dir, "wal", "current.wal") }

// crash simulates a power-loss after data was acknowledged: it fsyncs the WAL
// (the durability point), stops the background loops as a dead process would,
// and releases the OS file lock — but never flushes the head to a chunk and
// never truncates the WAL. Recovery must therefore come entirely from the log.
func crash(t *testing.T, db *TSDB) {
	t.Helper()
	if err := db.wal.Sync(); err != nil {
		t.Fatalf("sync before crash: %v", err)
	}
	db.cancel()  // no more background syncs/flushes, like a halted process
	db.wg.Wait() // join the loops so closing the WAL cannot race a Sync
	_ = db.wal.Close()
	if err := releaseLock(db.lock); err != nil {
		t.Fatalf("release lock after crash: %v", err)
	}
}

// walRecordOffsets returns the byte offset at which each record begins, by
// walking the length-prefixed frames. It is how the torn/corrupt tests land
// their damage on an exact record boundary.
func walRecordOffsets(t *testing.T, path string) []int64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var offs []int64
	for i := 0; i+walHeaderSize <= len(data); {
		offs = append(offs, int64(i))
		n := int(binary.BigEndian.Uint32(data[i : i+4]))
		i += walHeaderSize + n
	}
	return offs
}

// TestTSDB_CrashRecoveryReplaysAckedWAL: every point acknowledged and synced
// before a crash must be queryable after recovery, reconstructed purely from
// the WAL (no Close, no chunk flush).
func TestTSDB_CrashRecoveryReplaysAckedWAL(t *testing.T) {
	dir := t.TempDir()
	base := testutil.BaseTime

	db, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	const n = 500
	for i := 0; i < n; i++ {
		if err := db.Write(gauge("crash", float64(i), base.Add(time.Duration(i)*time.Millisecond))); err != nil {
			t.Fatal(err)
		}
	}
	crash(t, db)

	db2, err := Open(dir, testLogger())
	if err != nil {
		t.Fatalf("recovery open: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	got, err := db2.Query(storage.Query{Name: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("recovered %d points, want %d", len(got), n)
	}
	if got[0].Value != 0 || got[n-1].Value != float64(n-1) {
		t.Errorf("recovered values wrong: first=%v last=%v", got[0].Value, got[n-1].Value)
	}
}

// TestTSDB_TornWALKeepsSurvivingPrefix: a log truncated mid-record (crash while
// writing) must reopen cleanly with the intact prefix intact.
func TestTSDB_TornWALKeepsSurvivingPrefix(t *testing.T) {
	dir := t.TempDir()
	base := testutil.BaseTime

	db, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	const n = 10
	for i := 0; i < n; i++ {
		if err := db.Write(gauge("torn", float64(i), base.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	crash(t, db)

	path := walFile(dir)
	offs := walRecordOffsets(t, path)
	if len(offs) != n {
		t.Fatalf("found %d records, want %d", len(offs), n)
	}
	// Cut inside the last record's length header: the reader gets an unexpected
	// EOF and stops at the last complete record.
	if err := os.Truncate(path, offs[n-1]+4); err != nil {
		t.Fatal(err)
	}

	db2, err := Open(dir, testLogger())
	if err != nil {
		t.Fatalf("open after torn wal must succeed, got: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	got, err := db2.Query(storage.Query{Name: "torn"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n-1 {
		t.Fatalf("surviving prefix has %d points, want %d", len(got), n-1)
	}
	if got[n-2].Value != float64(n-2) {
		t.Errorf("last surviving value = %v, want %v", got[n-2].Value, float64(n-2))
	}
}

// TestTSDB_CorruptWALStopsAtBadRecord: a flipped payload byte fails its CRC;
// replay must stop cleanly at that record (keeping the records before it) rather
// than error the whole Open.
func TestTSDB_CorruptWALStopsAtBadRecord(t *testing.T) {
	dir := t.TempDir()
	base := testutil.BaseTime

	db, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	const n = 10
	for i := 0; i < n; i++ {
		if err := db.Write(gauge("corrupt", float64(i), base.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	crash(t, db)

	path := walFile(dir)
	offs := walRecordOffsets(t, path)
	if len(offs) != n {
		t.Fatalf("found %d records, want %d", len(offs), n)
	}
	const bad = 5
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[offs[bad]+walHeaderSize] ^= 0xFF // flip the first payload byte of record `bad`
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	db2, err := Open(dir, testLogger())
	if err != nil {
		t.Fatalf("open must not error on a corrupt record: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	got, err := db2.Query(storage.Query{Name: "corrupt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != bad {
		t.Fatalf("replay kept %d records, want %d (should stop at the corrupt one)", len(got), bad)
	}
	if got[bad-1].Value != float64(bad-1) {
		t.Errorf("last good value = %v, want %v", got[bad-1].Value, float64(bad-1))
	}
}

// TestTSDB_QueryAcrossChunkHeadBoundary flushes points to an on-disk chunk, then
// writes more (including a duplicate timestamp) to the WAL/head, reopens, and
// queries across the boundary. The merged result must be ordered, de-duplicated
// (head wins the shared timestamp), and lose nothing from either side.
func TestTSDB_QueryAcrossChunkHeadBoundary(t *testing.T) {
	dir := t.TempDir()
	base := testutil.BaseTime

	db, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ { // t0..t4 -> chunk
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
	// Fresh head + WAL: a duplicate of t2 (with a different value) plus t5, t6.
	if err := db.Write(gauge("m", 200, base.Add(2*time.Second))); err != nil {
		t.Fatal(err)
	}
	if err := db.Write(gauge("m", 5, base.Add(5*time.Second))); err != nil {
		t.Fatal(err)
	}
	if err := db.Write(gauge("m", 6, base.Add(6*time.Second))); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	got, err := db2.Query(storage.Query{Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{0, 1, 200, 3, 4, 5, 6} // t2 de-duped, head's 200 wins
	if len(got) != len(want) {
		t.Fatalf("merged query got %d points, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Value != w {
			t.Errorf("point %d value = %v, want %v", i, got[i].Value, w)
		}
		if i > 0 && !got[i-1].Timestamp.Before(got[i].Timestamp) {
			t.Errorf("points out of order at %d: %s then %s", i, got[i-1].Timestamp, got[i].Timestamp)
		}
	}
}

// TestTSDB_LockPreventsSecondOpenAndReleasesOnClose: the flock must reject a
// second Open promptly (it is non-blocking) and be released on Close so a later
// Open succeeds.
func TestTSDB_LockPreventsSecondOpenAndReleasesOnClose(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = Open(dir, testLogger())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("second Open of a locked dir should fail")
	}
	if elapsed > 3*time.Second {
		t.Errorf("second Open took %s; a non-blocking flock must fail promptly", elapsed)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(dir, testLogger())
	if err != nil {
		t.Fatalf("Open after Close should succeed (lock released): %v", err)
	}
	_ = db2.Close()
}
