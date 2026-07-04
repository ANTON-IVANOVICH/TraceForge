package storage

import (
	"testing"
	"time"

	"metrics-system/internal/model"
)

func TestSeriesKeyCanonical(t *testing.T) {
	k1 := seriesKey("cpu", map[string]string{"b": "2", "a": "1"})
	k2 := seriesKey("cpu", map[string]string{"a": "1", "b": "2"})
	if k1 != k2 {
		t.Fatalf("keys differ regardless of insertion order: %q vs %q", k1, k2)
	}
	if want := "cpu{a=1,b=2}"; k1 != want {
		t.Errorf("key = %q, want %q", k1, want)
	}
	if k := seriesKey("cpu", nil); k != "cpu" {
		t.Errorf("no-label key = %q, want cpu", k)
	}
}

func TestMemoryStorage_WriteQueryRaw(t *testing.T) {
	s := NewMemoryStorage()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		s.Write(model.Metric{
			Name:      "cpu",
			Type:      model.MetricTypeGauge,
			Value:     float64(i),
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Labels:    map[string]string{"host": "a"},
		})
	}

	got, err := s.Query(Query{Name: "cpu"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d points, want 3", len(got))
	}
	if got[0].Value != 0 || got[2].Value != 2 {
		t.Errorf("values = %v/%v, want 0/2", got[0].Value, got[2].Value)
	}
	if got[0].Type != model.MetricTypeGauge {
		t.Error("metric type not preserved through storage")
	}
}

func TestMemoryStorage_LabelFilter(t *testing.T) {
	s := NewMemoryStorage()
	now := time.Now()
	s.Write(model.Metric{Name: "cpu", Type: model.MetricTypeGauge, Value: 1, Timestamp: now, Labels: map[string]string{"host": "a"}})
	s.Write(model.Metric{Name: "cpu", Type: model.MetricTypeGauge, Value: 2, Timestamp: now, Labels: map[string]string{"host": "b"}})

	got, _ := s.Query(Query{Name: "cpu", Labels: map[string]string{"host": "b"}})
	if len(got) != 1 || got[0].Value != 2 {
		t.Fatalf("label filter got %+v, want single value 2", got)
	}
	if st := s.Stats(); st.Series != 2 || st.Points != 2 {
		t.Errorf("stats = %+v, want series=2 points=2", st)
	}
}

func TestMemoryStorage_Aggregation(t *testing.T) {
	s := NewMemoryStorage()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, v := range []float64{10, 20, 30, 40} {
		s.Write(model.Metric{Name: "cpu", Type: model.MetricTypeGauge, Value: v, Timestamp: base.Add(time.Duration(i) * time.Second)})
	}

	got, err := s.Query(Query{Name: "cpu", Aggregator: avgAgg{}, From: base, To: base.Add(time.Hour), Step: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d windows, want 1", len(got))
	}
	if got[0].Value != 25 {
		t.Errorf("avg = %v, want 25", got[0].Value)
	}
}

func TestAggregators(t *testing.T) {
	pts := []Point{{Value: 1}, {Value: 2}, {Value: 3}, {Value: 4}}
	cases := []struct {
		agg  Aggregator
		want float64
	}{
		{avgAgg{}, 2.5},
		{minAgg{}, 1},
		{maxAgg{}, 4},
		{sumAgg{}, 10},
		{countAgg{}, 4},
		{percentileAgg{p: 0.5}, 2},  // idx = int(3*0.5) = 1 -> sorted[1] = 2
		{percentileAgg{p: 0.99}, 3}, // idx = int(3*0.99) = 2 -> sorted[2] = 3
	}
	for _, c := range cases {
		if got := c.agg.Aggregate(pts); got != c.want {
			t.Errorf("%s = %v, want %v", c.agg.Name(), got, c.want)
		}
	}
}

func TestFilterTime(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pts := []Point{
		{Timestamp: base, Value: 0},
		{Timestamp: base.Add(time.Minute), Value: 1},
		{Timestamp: base.Add(2 * time.Minute), Value: 2},
	}
	got := filterTime(pts, base.Add(time.Minute), time.Time{})
	if len(got) != 2 || got[0].Value != 1 {
		t.Errorf("filterTime(from) = %+v, want the last two points", got)
	}
	if all := filterTime(pts, time.Time{}, time.Time{}); len(all) != 3 {
		t.Errorf("filterTime(open) dropped points: %d", len(all))
	}
}

func TestQuery_MissingName(t *testing.T) {
	if _, err := NewMemoryStorage().Query(Query{}); err == nil {
		t.Error("expected error for missing query name")
	}
}
