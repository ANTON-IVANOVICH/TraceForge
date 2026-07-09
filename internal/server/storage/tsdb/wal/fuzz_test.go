package wal

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// FuzzReplay drives Replay against two kinds of input and asserts two different
// invariants:
//
//  1. Robustness. The raw fuzz bytes are written verbatim to a log file and
//     replayed. Replay documents that a missing file, a torn header/payload, a
//     bad CRC and an oversized length all end *cleanly* (return nil); the only
//     non-nil return it documents is a genuine read error, which a regular file
//     cannot produce mid-stream. So for any file content, with a handler that
//     never errors, Replay must return nil and must never panic. That is exactly
//     the property the 4 GiB-allocation bug violated in spirit — a hostile length
//     is now clamped instead of trusted.
//
//  2. Prefix property. The fuzz bytes are *also* decoded into a list of payloads,
//     written through the real Write path into a valid log, then optionally
//     corrupted (one byte flipped) and truncated. Whatever Replay yields from the
//     damaged log must be a prefix of the payloads that were written: damage can
//     only make Replay return fewer records, never a different or extra one. A
//     single-byte flip can never forge a record — CRC32 detects every single-byte
//     change, and the length/CRC fields are independent — so this holds by
//     construction and any violation is a real bug in Replay.
func FuzzReplay(f *testing.F) {
	f.Add([]byte{}, uint16(0), uint16(0), byte(0))
	f.Add([]byte{3, 'o', 'n', 'e'}, uint16(0), uint16(0), byte(0))
	f.Add([]byte{3, 'o', 'n', 'e', 3, 't', 'w', 'o'}, uint16(0), uint16(0), byte(0))
	f.Add([]byte{3, 'o', 'n', 'e', 3, 't', 'w', 'o'}, uint16(9), uint16(0), byte(0))     // truncate mid record 2
	f.Add([]byte{3, 'o', 'n', 'e', 3, 't', 'w', 'o'}, uint16(0), uint16(13), byte(0xFF)) // flip record 2's payload
	f.Add([]byte{0}, uint16(0), uint16(0), byte(0))                                      // one empty payload
	f.Add([]byte{2, 'h', 'i', 5, 'w', 'o', 'r', 'l', 'd'}, uint16(1), uint16(2), byte(1))

	f.Fuzz(func(t *testing.T, raw []byte, cut uint16, flipAt uint16, flipVal byte) {
		// (1) robustness on arbitrary bytes.
		robustPath := filepath.Join(t.TempDir(), "raw.wal")
		if err := os.WriteFile(robustPath, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Replay(robustPath, func([]byte) error { return nil }); err != nil {
			t.Fatalf("Replay of arbitrary bytes returned a non-nil error: %v", err)
		}

		// (2) prefix property on a valid-then-damaged log.
		payloads, ok := decodePayloads(raw)
		if !ok {
			return
		}
		buf := buildLog(t, payloads)
		if flipVal != 0 && len(buf) > 0 {
			buf[int(flipAt)%len(buf)] ^= flipVal // guaranteed change: v ^ flipVal != v
		}
		if len(buf) > 0 {
			buf = buf[:int(cut)%(len(buf)+1)]
		}

		got := replayBytes(t, buf)
		if len(got) > len(payloads) {
			t.Fatalf("Replay yielded %d records, more than the %d written", len(got), len(payloads))
		}
		for i := range got {
			if !bytes.Equal(got[i], payloads[i]) {
				t.Fatalf("record %d is not a prefix of what was written:\n got  %q\n want %q", i, got[i], payloads[i])
			}
		}
	})
}

// decodePayloads reads a length-prefixed list of payloads out of fuzz bytes so
// the fuzzer explores sets of records rather than only single blobs. Each record
// is one length byte (0..255) followed by that many payload bytes; a truncated
// tail makes the whole input un-decodable, which the caller skips.
func decodePayloads(data []byte) ([][]byte, bool) {
	var out [][]byte
	for len(data) > 0 {
		if len(out) >= 64 { // bound the work per fuzz iteration
			return nil, false
		}
		n := int(data[0])
		data = data[1:]
		if len(data) < n {
			return nil, false
		}
		out = append(out, append([]byte(nil), data[:n]...))
		data = data[n:]
	}
	return out, true
}

// buildLog writes payloads through the real Write path and returns the raw log
// bytes, so the fuzzer damages exactly the on-disk format the writer produces.
func buildLog(t *testing.T, payloads [][]byte) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seed.wal")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range payloads {
		if err := w.Write(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// replayBytes writes buf to a fresh file and returns every payload Replay yields.
func replayBytes(t *testing.T, buf []byte) [][]byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "replay.wal")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	var got [][]byte
	if err := Replay(path, func(p []byte) error {
		got = append(got, append([]byte(nil), p...))
		return nil
	}); err != nil {
		t.Fatalf("Replay of a valid-then-damaged log errored: %v", err)
	}
	return got
}
