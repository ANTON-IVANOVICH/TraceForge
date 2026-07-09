package chunk

import (
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
)

// No build tag: `go test` compiles these but runs them only under -bench.

// Sinks keep results reachable so the compiler cannot delete the calls under test.
var (
	sinkPoints []storage.Point
	sinkReader *Reader
)

var benchBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// benchSeries builds nSeries distinct series of nPoints each, at one-second
// spacing — a realistic head snapshot rather than one hot series.
func benchSeries(nSeries, nPoints int) []storage.Series {
	out := make([]storage.Series, nSeries)
	for s := range out {
		pts := make([]storage.Point, nPoints)
		for p := range pts {
			pts[p] = storage.Point{Timestamp: benchBase.Add(time.Duration(p) * time.Second), Value: float64(s*nPoints + p)}
		}
		out[s] = storage.Series{
			Name:   fmt.Sprintf("series_%d", s),
			Type:   model.MetricTypeGauge,
			Labels: map[string]string{"host": fmt.Sprintf("web-%d", s%8)},
			Points: pts,
		}
	}
	return out
}

// BenchmarkChunkWrite measures the serialize + fsync + atomic-rename cost as the
// series count grows. Each iteration writes to a fresh directory because Write
// renames onto its target and cannot overwrite a populated one.
func BenchmarkChunkWrite(b *testing.B) {
	for _, nSeries := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("series=%d", nSeries), func(b *testing.B) {
			series := benchSeries(nSeries, 100)
			base := b.TempDir()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := Write(filepath.Join(base, strconv.Itoa(i)), series); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkChunkReadSeries contrasts a full scan against a narrow window over the
// same 10k-point series. The narrow window sits at the far end, so the binary
// search skips almost everything the full scan touches — the gap is the search
// paying for itself.
func BenchmarkChunkReadSeries(b *testing.B) {
	const n = 10_000
	dir := filepath.Join(b.TempDir(), "chunk")
	if err := Write(dir, benchSeries(1, n)); err != nil {
		b.Fatal(err)
	}
	r, err := Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	key := storage.SeriesKey("series_0", map[string]string{"host": "web-0"})

	b.Run("full", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			pts, err := r.ReadSeries(key, time.Time{}, time.Time{})
			if err != nil {
				b.Fatal(err)
			}
			sinkPoints = pts
		}
	})
	b.Run("range", func(b *testing.B) {
		from := benchBase.Add(time.Duration(n-10) * time.Second)
		to := benchBase.Add(time.Duration(n) * time.Second)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			pts, err := r.ReadSeries(key, from, to)
			if err != nil {
				b.Fatal(err)
			}
			sinkPoints = pts
		}
	})
}

// BenchmarkChunkOpen measures the open path: read + parse index.json and map the
// data file. Each iteration closes the reader so mmaps and descriptors are not
// leaked across the loop.
func BenchmarkChunkOpen(b *testing.B) {
	dir := filepath.Join(b.TempDir(), "chunk")
	if err := Write(dir, benchSeries(100, 100)); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, err := Open(dir)
		if err != nil {
			b.Fatal(err)
		}
		sinkReader = r
		if err := r.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
