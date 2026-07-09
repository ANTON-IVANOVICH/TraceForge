package chunk

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
)

func TestChunk_WriteReadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chunk01")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	series := []storage.Series{
		{Name: "cpu", Type: model.MetricTypeGauge, Labels: map[string]string{"host": "a"}, Points: []storage.Point{
			{Timestamp: base, Value: 1},
			{Timestamp: base.Add(time.Second), Value: 2},
			{Timestamp: base.Add(2 * time.Second), Value: 3},
		}},
		{Name: "mem", Type: model.MetricTypeGauge, Points: []storage.Point{{Timestamp: base, Value: 99}}},
	}
	if err := Write(dir, series); err != nil {
		t.Fatal(err)
	}

	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	key := storage.SeriesKey("cpu", map[string]string{"host": "a"})
	pts, err := r.ReadSeries(key, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 3 || pts[0].Value != 1 || pts[2].Value != 3 {
		t.Fatalf("cpu points = %+v, want [1,2,3]", pts)
	}

	// Binary-search range: exactly the middle point.
	pts2, err := r.ReadSeries(key, base.Add(time.Second), base.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(pts2) != 1 || pts2[0].Value != 2 {
		t.Fatalf("range read = %+v, want [2]", pts2)
	}

	metas := 0
	r.ForEachSeries(func(SeriesMeta) { metas++ })
	if metas != 2 {
		t.Fatalf("series meta count = %d, want 2", metas)
	}
	if !r.MinTime().Equal(base) || !r.MaxTime().Equal(base.Add(2*time.Second)) {
		t.Errorf("time span = [%v,%v]", r.MinTime(), r.MaxTime())
	}
}

func TestChunk_WriteIsAtomic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "c")
	if err := Write(dir, []storage.Series{
		{Name: "x", Type: model.MetricTypeGauge, Points: []storage.Point{{Timestamp: time.Now(), Value: 1}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp dir should be removed after atomic rename")
	}
	if _, err := os.Stat(filepath.Join(dir, "data")); err != nil {
		t.Errorf("data file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err != nil {
		t.Errorf("index.json missing: %v", err)
	}
}

// TestReadSeries_CorruptIndexBoundsRejected is the deterministic sibling of
// FuzzReadSeriesCorruptIndex: it hand-writes an index.json whose length field
// overflows int64 addition. Before the bounds check was rewritten to compare
// each term against len(data) separately, Offset+Length wrapped negative, sailed
// past `> len(data)`, and the slice expression panicked with an out-of-range
// index on a plain query.
func TestReadSeries_CorruptIndexBoundsRejected(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	indexes := map[string]string{
		"length overflows int64": `{"series":{"cpu":{"name":"cpu","type":"gauge","offset":9,"length":9223372036854775807}},"min_time":0,"max_time":0,"points":1}`,
		"offset past data":       `{"series":{"cpu":{"name":"cpu","type":"gauge","offset":1000000,"length":16}},"min_time":0,"max_time":0,"points":1}`,
		"offset before header":   `{"series":{"cpu":{"name":"cpu","type":"gauge","offset":0,"length":16}},"min_time":0,"max_time":0,"points":1}`,
	}
	for name, idx := range indexes {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "chunk")
			if err := Write(dir, []storage.Series{
				{Name: "cpu", Type: model.MetricTypeGauge, Points: []storage.Point{{Timestamp: base, Value: 1}}},
			}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(idx), 0o644); err != nil {
				t.Fatal(err)
			}
			r, err := Open(dir)
			if err != nil {
				return // a rejected index is a fine outcome
			}
			defer func() { _ = r.Close() }()
			if _, err := r.ReadSeries("cpu", time.Time{}, time.Time{}); err == nil {
				t.Fatal("out-of-bounds series entry must return an error, not read")
			}
		})
	}
}
