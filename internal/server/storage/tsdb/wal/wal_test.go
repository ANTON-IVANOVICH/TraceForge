package wal

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWAL_WriteReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.wal")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	payloads := []string{"one", "two", "three"}
	for _, p := range payloads {
		if err := w.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	var got []string
	if err := Replay(path, func(p []byte) error {
		got = append(got, string(p))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(payloads) {
		t.Fatalf("replayed %d records, want %d", len(got), len(payloads))
	}
	for i := range payloads {
		if got[i] != payloads[i] {
			t.Errorf("record %d = %q, want %q", i, got[i], payloads[i])
		}
	}
}

func TestWAL_ReplayMissingFile(t *testing.T) {
	if err := Replay(filepath.Join(t.TempDir(), "nope.wal"), func([]byte) error { return nil }); err != nil {
		t.Errorf("replay of a missing file should be nil, got %v", err)
	}
}

func TestWAL_TornRecordTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "torn.wal")
	w, _ := Open(path)
	_ = w.Write([]byte("good"))
	_ = w.Sync()
	_ = w.Close()

	// Append a torn record: a header claiming a 100-byte payload, but no payload.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	var hdr [headerSize]byte
	binary.BigEndian.PutUint32(hdr[0:4], 100)
	_, _ = f.Write(hdr[:])
	_ = f.Close()

	var count int
	if err := Replay(path, func([]byte) error { count++; return nil }); err != nil {
		t.Fatalf("torn record should be tolerated, got %v", err)
	}
	if count != 1 {
		t.Fatalf("recovered %d records, want 1 (torn record dropped)", count)
	}
}

// A header claiming a 4 GiB payload must not be trusted: reaching the
// make([]byte, length) would allocate 4 GiB on startup from a single corrupt
// record. maxRecordSize turns it into a clean stop with no records.
func TestWAL_ReplayOversizedLengthDoesNotAllocate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.wal")
	var hdr [headerSize]byte
	binary.BigEndian.PutUint32(hdr[0:4], 0xFFFFFFFF) // a header claiming ~4 GiB
	if err := os.WriteFile(path, hdr[:], 0o644); err != nil {
		t.Fatal(err)
	}

	// "Does it OOM" is the wrong test: make([]byte, 4GiB) hands back lazily-mapped
	// zero pages that are never faulted, so RSS never moves and the process never
	// dies — the assertion would pass with the guard removed. What *does* move is
	// the allocator's accounting: TotalAlloc counts the requested bytes whether or
	// not they are touched. So without the guard this replay bumps TotalAlloc by
	// ~4 GiB, and with it by a few kilobytes — a difference the test can see.
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	var count int
	if err := Replay(path, func([]byte) error { count++; return nil }); err != nil {
		t.Fatalf("an oversized length should stop replay cleanly, got %v", err)
	}

	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	if count != 0 {
		t.Fatalf("recovered %d records from a lone oversized header, want 0", count)
	}
	// The bufio reader plus the header buffer are a few tens of KiB; 16 MiB leaves
	// enormous headroom below the 4 GiB an ungated make() would record.
	if allocated > 16<<20 {
		t.Errorf("replaying an oversized-length header allocated %d MiB; the length guard is not stopping the make()",
			allocated>>20)
	}
}

// A bad CRC anywhere in the log ends replay: records before it are recovered,
// records after it are not (they might be built on torn state).
func TestWAL_ReplayStopsAtCorruptCRCMidLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.wal")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	payloads := []string{"r0", "r1", "r2", "r3", "r4"}
	for _, p := range payloads {
		if err := w.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Every payload is 2 bytes, so records are fixed-width. Flip a CRC byte of the
	// middle record (index 2); records 0 and 1 must still replay, 2..4 must not.
	const recSize = headerSize + 2
	crcOffset := int64(2*recSize + 4) // record 2, into its crc(4) field
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xAA}, crcOffset); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	var got []string
	if err := Replay(path, func(p []byte) error { got = append(got, string(p)); return nil }); err != nil {
		t.Fatalf("corrupt CRC mid-log should stop cleanly, got %v", err)
	}
	if len(got) != 2 || got[0] != "r0" || got[1] != "r1" {
		t.Fatalf("replay after mid-log corruption = %v, want [r0 r1]", got)
	}
}

func TestWAL_TruncateClears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.wal")
	w, _ := Open(path)
	_ = w.Write([]byte("x"))
	_ = w.Sync()
	if err := w.Truncate(); err != nil {
		t.Fatal(err)
	}
	if w.Size() != 0 {
		t.Errorf("size after truncate = %d, want 0", w.Size())
	}
	_ = w.Write([]byte("y"))
	_ = w.Sync()
	_ = w.Close()

	var got []string
	_ = Replay(path, func(p []byte) error { got = append(got, string(p)); return nil })
	if len(got) != 1 || got[0] != "y" {
		t.Fatalf("after truncate, replay = %v, want [y]", got)
	}
}

// Write must refuse a record Replay would refuse to read: writing one would be a
// point that vanishes on the next restart, silently.
func TestWALWriteRejectsOversizedRecord(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	if err := w.Write(make([]byte, maxRecordSize+1)); err == nil {
		t.Fatal("Write accepted a record larger than maxRecordSize; Replay would drop it")
	}
	// A record exactly at the limit is fine.
	if err := w.Write(make([]byte, maxRecordSize)); err != nil {
		t.Errorf("Write rejected a record at the limit: %v", err)
	}
}
