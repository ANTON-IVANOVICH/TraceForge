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
	defer r.Close()

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

func FuzzParseHeader(f *testing.F) {
	f.Add([]byte("TSDB\x01"))
	f.Add([]byte(""))
	f.Add([]byte("XXXX\x00"))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_ = ParseHeader(data) // must never panic
	})
}
