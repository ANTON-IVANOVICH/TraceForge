package tsdb

import (
	"fmt"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
	"metrics-system/internal/testutil"
)

// No build tag: `go test` compiles these but runs them only under -bench.

var sinkMetrics []model.Metric

// BenchmarkHeadWrite measures the in-memory ingest path in isolation — no WAL,
// no fsync, just the map lookup, append and min/max tracking that every write
// pays. It writes a realistic 512-series working set rather than one hot key,
// which would only exercise the L1 cache.
func BenchmarkHeadWrite(b *testing.B) {
	metrics := testutil.Metrics("bench", 512)
	h := newHead()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.write(metrics[i%len(metrics)])
	}
}

// BenchmarkTSDBWriteBatch measures the full durable write path (WAL append +
// head write) as batch size grows, showing the per-metric cost amortised over
// one lock acquisition. Sync is left to the background loop, as in production.
func BenchmarkTSDBWriteBatch(b *testing.B) {
	for _, size := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("batch=%d", size), func(b *testing.B) {
			metrics := testutil.Metrics("bench", size)
			db, err := Open(b.TempDir(), testLogger())
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = db.Close() }()

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := db.WriteBatch(metrics); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkTSDBQuery contrasts a query served entirely from the head against one
// that must also scan a chunk and de-duplicate the overlap — the merge cost that
// the LSM design trades for cheap writes.
func BenchmarkTSDBQuery(b *testing.B) {
	base := testutil.BaseTime

	// head-only: all points live in the in-memory head.
	headOnly, err := Open(b.TempDir(), testLogger())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = headOnly.Close() }()
	writeSeries(b, headOnly, "cpu", base, 0, 10_000)

	// head+chunks: half the points flushed to a chunk, half kept in a fresh head.
	mixed, err := Open(b.TempDir(), testLogger())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = mixed.Close() }()
	writeSeries(b, mixed, "cpu", base, 0, 5_000)
	if err := mixed.flush(); err != nil {
		b.Fatal(err)
	}
	writeSeries(b, mixed, "cpu", base, 5_000, 10_000)

	q := storage.Query{Name: "cpu", From: base, To: base.Add(20_000 * time.Second)}
	for _, tc := range []struct {
		name string
		db   *TSDB
	}{{"head-only", headOnly}, {"head+chunks", mixed}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := tc.db.Query(q)
				if err != nil {
					b.Fatal(err)
				}
				sinkMetrics = out
			}
		})
	}
}

// writeSeries writes points [from,to) of one gauge series at one-second spacing.
func writeSeries(b *testing.B, db *TSDB, name string, base time.Time, from, to int) {
	b.Helper()
	for i := from; i < to; i++ {
		if err := db.Write(gauge(name, float64(i), base.Add(time.Duration(i)*time.Second))); err != nil {
			b.Fatal(err)
		}
	}
}
