package wal

import (
	"fmt"
	"path/filepath"
	"testing"
)

// Benchmarks carry no build tag: `go test` compiles them but runs them only
// under -bench, so a tag would buy nothing and let them rot out of CI's compile.

// Package-level sinks defeat dead-code elimination: without a reachable use of a
// call's result the compiler is free to drop the call and the loop measures air.
var (
	sinkErr error
	sinkInt int
)

// BenchmarkWALWrite measures the buffered append path (no fsync) across the
// payload sizes the pipeline actually produces — a JSON metric is tens to a few
// hundred bytes. SetBytes reports throughput per size.
func BenchmarkWALWrite(b *testing.B) {
	for _, size := range []int{64, 512, 4096} {
		b.Run(fmt.Sprintf("payload=%d", size), func(b *testing.B) {
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte(i)
			}
			w, err := Open(filepath.Join(b.TempDir(), "bench.wal"))
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = w.Close() }()

			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sinkErr = w.Write(payload)
			}
			b.StopTimer()
			if sinkErr != nil {
				b.Fatal(sinkErr)
			}
		})
	}
}

// BenchmarkWALWriteSync isolates the durability cost: one fsync per record. The
// gap between this and BenchmarkWALWrite/payload=512 is the price of durability,
// and the reason writes are batched behind a periodic Sync in the TSDB.
func BenchmarkWALWriteSync(b *testing.B) {
	payload := make([]byte, 512)
	w, err := Open(filepath.Join(b.TempDir(), "sync.wal"))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := w.Write(payload); err != nil {
			b.Fatal(err)
		}
		if err := w.Sync(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReplay measures recovery: reading and CRC-checking a full log. The
// log of 10k records is built once, outside the timer, then replayed each
// iteration with a handler that only touches the payload so it is not elided.
func BenchmarkReplay(b *testing.B) {
	path := filepath.Join(b.TempDir(), "replay.wal")
	w, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	payload := make([]byte, 256)
	for i := 0; i < 10_000; i++ {
		payload[i%len(payload)] = byte(i) // vary content so CRC input is not constant
		if err := w.Write(payload); err != nil {
			b.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := Replay(path, func(p []byte) error { sinkInt += len(p); return nil }); err != nil {
			b.Fatal(err)
		}
	}
}
