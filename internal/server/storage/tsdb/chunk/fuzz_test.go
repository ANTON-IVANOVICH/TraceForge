package chunk

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
)

// FuzzParseHeader asserts the header validator never panics on arbitrary bytes —
// it runs against the first five bytes of every mmap'd data file, which after a
// crash or a corrupt write can be anything at all.
func FuzzParseHeader(f *testing.F) {
	f.Add([]byte("TSDB\x01")) // valid
	f.Add([]byte(""))
	f.Add([]byte("TSDB"))         // one byte short of a header
	f.Add([]byte("TSD"))          // shorter than the magic
	f.Add([]byte("XXXX\x00"))     // wrong magic
	f.Add([]byte("TSDB\x02"))     // unsupported version
	f.Add([]byte("TSDB\x01\x00")) // valid header plus a trailing byte
	f.Add([]byte{0, 0, 0, 0, 0})
	f.Fuzz(func(_ *testing.T, data []byte) {
		_ = ParseHeader(data) // must never panic
	})
}

// FuzzChunkRoundTrip fuzzes whole chunks: the bytes are decoded into a set of
// series with distinct-timestamp points, written to disk, reopened and read back.
// The invariant is conservation — every point written comes back exactly once, in
// ascending timestamp order, with a bit-identical value. Comparing float *bits*
// rather than floats is deliberate: NaN != NaN and -0.0 == 0.0 under ==, so a
// value comparison would either spuriously fail on a NaN that survived intact or
// silently accept a sign flip on zero. Bits make both honest.
func FuzzChunkRoundTrip(f *testing.F) {
	// One series, two points.
	f.Add([]byte{1, 2, 0, 0, 0, 10, 0, 0, 0, 0, 0, 0, 0, 40, 0, 0, 0, 5, 0, 0, 0, 0, 0, 0, 0, 99})
	// Two series.
	f.Add([]byte{2,
		1, 0, 0, 0, 1, 0x40, 0x59, 0, 0, 0, 0, 0, 0, // series 0: 1 pt, value 100.0
		1, 0, 0, 0, 2, 0x7f, 0xf8, 0, 0, 0, 0, 0, 0, // series 1: 1 pt, value NaN
	})
	f.Add([]byte{}) // zero series: an empty chunk must still open

	f.Fuzz(func(t *testing.T, data []byte) {
		series, expected := decodeSeriesSet(data)

		dir := filepath.Join(t.TempDir(), "chunk")
		if err := Write(dir, series); err != nil {
			t.Fatalf("Write of decoded series failed: %v", err)
		}
		r, err := Open(dir)
		if err != nil {
			t.Fatalf("Open of a freshly written chunk failed: %v", err)
		}
		defer func() { _ = r.Close() }()

		for key, want := range expected {
			got, err := r.ReadSeries(key, time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("ReadSeries(%q): %v", key, err)
			}
			if len(got) != len(want) {
				t.Fatalf("series %q: read %d points, wrote %d", key, len(got), len(want))
			}
			var prev int64
			for i, p := range got {
				ns := p.Timestamp.UnixNano()
				if i > 0 && ns < prev {
					t.Fatalf("series %q: point %d ts %d precedes previous %d — not ascending", key, i, ns, prev)
				}
				prev = ns
				if ns != want[i].ns {
					t.Fatalf("series %q point %d: ts %d, want %d", key, i, ns, want[i].ns)
				}
				if gb, wb := math.Float64bits(p.Value), want[i].bits; gb != wb {
					t.Fatalf("series %q point %d: value bits %#016x, want %#016x", key, i, gb, wb)
				}
			}
		}
	})
}

// FuzzReadSeriesCorruptIndex writes a valid chunk, replaces its index.json with
// fuzz bytes, and drives Open plus ReadSeries over whatever series the corrupt
// index claims. Open may reject the index (fine); if it does not, no read may
// panic or run off the end of the mapped data. The overflow seed exercises the
// int64 bounds check directly.
func FuzzReadSeriesCorruptIndex(f *testing.F) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.Add([]byte(`{"series":{"cpu":{"name":"cpu","type":"gauge","offset":9,"length":16}},"min_time":0,"max_time":0,"points":1}`))
	f.Add([]byte(`{"series":{"cpu":{"name":"cpu","type":"gauge","offset":9,"length":9223372036854775807}},"min_time":0,"max_time":0,"points":1}`))
	f.Add([]byte(`{"series":{"cpu":{"name":"cpu","type":"gauge","offset":-1,"length":16}}}`))
	f.Add([]byte(`{"series":{}}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, indexBytes []byte) {
		dir := filepath.Join(t.TempDir(), "chunk")
		if err := Write(dir, []storage.Series{
			{Name: "cpu", Type: model.MetricTypeGauge, Points: []storage.Point{
				{Timestamp: base, Value: 1},
				{Timestamp: base.Add(time.Second), Value: 2},
			}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.json"), indexBytes, 0o644); err != nil {
			t.Fatal(err)
		}

		r, err := Open(dir)
		if err != nil {
			return // corrupt index rejected cleanly
		}
		defer func() { _ = r.Close() }()

		var keys []string
		r.ForEachSeries(func(m SeriesMeta) { keys = append(keys, m.Key) })
		keys = append(keys, "cpu", "", "missing")
		for _, key := range keys {
			// Must not panic or read out of bounds for open or bounded windows.
			_, _ = r.ReadSeries(key, time.Time{}, time.Time{})
			_, _ = r.ReadSeries(key, base, base.Add(time.Hour))
		}
	})
}

// expectedPoint is one point on the conservation side: the exact nanosecond
// timestamp and the exact value bits that must survive the round trip.
type expectedPoint struct {
	ns   int64
	bits uint64
}

// decodeSeriesSet turns fuzz bytes into a set of series with points, plus the
// per-series expectation ReadSeries must reproduce. Series are given synthetic,
// distinct names so two of them can never collapse to one key inside Write.
// Points within a series are keyed by timestamp so every timestamp is distinct —
// otherwise Write's non-stable sort could reorder equal-timestamp points and
// there would be no single correct read-back order to assert against.
//
// Layout: an optional leading series count, then for each series a point count
// followed by that many 12-byte points (4-byte timestamp offset + 8-byte value
// bits). Timestamp offsets are added to a fixed base so every instant stays in
// the range time.UnixNano round-trips.
func decodeSeriesSet(data []byte) ([]storage.Series, map[string][]expectedPoint) {
	const base = 1_767_225_600_000_000_000 // 2026-01-01T00:00:00Z in ns
	series := []storage.Series{}
	expected := map[string][]expectedPoint{}

	nSeries := 4
	if len(data) > 0 {
		nSeries = int(data[0])%8 + 1
		data = data[1:]
	}

	for s := 0; s < nSeries && len(data) > 0; s++ {
		nPoints := int(data[0]) % 64
		data = data[1:]

		name := fmt.Sprintf("series_%d", s)
		byTS := map[int64]uint64{} // ts -> value bits, dedups timestamps
		for p := 0; p < nPoints && len(data) >= 12; p++ {
			ns := base + int64(binary.BigEndian.Uint32(data[0:4]))
			bits := binary.BigEndian.Uint64(data[4:12])
			data = data[12:]
			byTS[ns] = bits
		}

		pts := make([]storage.Point, 0, len(byTS))
		exp := make([]expectedPoint, 0, len(byTS))
		for ns, bits := range byTS {
			pts = append(pts, storage.Point{Timestamp: time.Unix(0, ns).UTC(), Value: math.Float64frombits(bits)})
		}
		series = append(series, storage.Series{Name: name, Type: model.MetricTypeGauge, Points: pts})

		nss := make([]int64, 0, len(byTS))
		for ns := range byTS {
			nss = append(nss, ns)
		}
		sort.Slice(nss, func(i, j int) bool { return nss[i] < nss[j] })
		for _, ns := range nss {
			exp = append(exp, expectedPoint{ns: ns, bits: byTS[ns]})
		}
		expected[storage.SeriesKey(name, nil)] = exp
	}
	return series, expected
}
