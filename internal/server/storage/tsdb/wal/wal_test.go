package wal

import (
	"encoding/binary"
	"os"
	"path/filepath"
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
