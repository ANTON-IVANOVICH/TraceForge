package bolt

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
)

func TestBoltStorage_PersistAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.bolt")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := s.Write(model.Metric{
			Name: "cpu", Type: model.MetricTypeGauge, Value: float64(i),
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Labels:    map[string]string{"host": "a"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen a fresh handle to the same file — data must survive.
	s2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()

	got, err := s2.Query(storage.Query{Name: "cpu"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d points after reopen, want 5", len(got))
	}
	if got[0].Value != 0 || got[4].Value != 4 {
		t.Errorf("values wrong after reopen: %v / %v", got[0].Value, got[4].Value)
	}
	if got[0].Labels["host"] != "a" {
		t.Errorf("labels lost across reopen: %v", got[0].Labels)
	}
	if got[0].Type != model.MetricTypeGauge {
		t.Errorf("type lost across reopen")
	}
}

func TestBoltStorage_RangeAndAggregation(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "t.bolt"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, v := range []float64{10, 20, 30, 40} {
		if err := s.Write(model.Metric{Name: "m", Type: model.MetricTypeGauge, Value: v, Timestamp: base.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}

	// Range [t+1s, t+2s] -> the two middle points (20, 30).
	got, err := s.Query(storage.Query{Name: "m", From: base.Add(time.Second), To: base.Add(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Value != 20 || got[1].Value != 30 {
		t.Fatalf("range scan got %+v, want [20,30]", got)
	}

	// avg over one big window.
	agg, err := storage.AggregatorByName("avg")
	if err != nil {
		t.Fatal(err)
	}
	got2, err := s.Query(storage.Query{Name: "m", Aggregator: agg, From: base, To: base.Add(time.Hour), Step: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 1 || got2[0].Value != 25 {
		t.Fatalf("avg got %+v, want single 25", got2)
	}
}

func TestBoltStorage_LabelFilterAndStats(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "t.bolt"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	now := time.Now().UTC()
	_ = s.WriteBatch([]model.Metric{
		{Name: "cpu", Type: model.MetricTypeGauge, Value: 1, Timestamp: now, Labels: map[string]string{"host": "a"}},
		{Name: "cpu", Type: model.MetricTypeGauge, Value: 2, Timestamp: now.Add(time.Second), Labels: map[string]string{"host": "b"}},
	})

	got, err := s.Query(storage.Query{Name: "cpu", Labels: map[string]string{"host": "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Value != 2 {
		t.Fatalf("label filter got %+v, want single value 2", got)
	}
	if st := s.Stats(); st.Series != 2 || st.Points != 2 {
		t.Errorf("stats = %+v, want series=2 points=2", st)
	}
}

// Ping is what the readiness probe calls. It has to distinguish a database that
// is open and whole from one that is not, and it has to do so without a write.
func TestBoltStorage_Ping(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "ping.bolt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Errorf("Ping on an open database: %v", err)
	}

	// A closed database is the state a probe must catch: bbolt refuses every
	// transaction, and nothing else in the process would notice until the next
	// write.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(context.Background()); err == nil {
		t.Error("Ping reports a closed database as healthy")
	}
}

// Readers in bbolt take a snapshot of the meta page and never queue behind the
// single writer. That property is the whole reason Ping uses a View transaction,
// so it is worth pinning: a probe that waits for a slow write is a probe that
// fails a healthy replica under load.
func TestBoltStorage_PingDoesNotWaitForTheWriter(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "ping-writer.bolt"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	writing := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = s.db.Update(func(*bbolt.Tx) error {
			close(writing)
			<-release
			return nil
		})
	}()
	<-writing
	defer close(release)

	done := make(chan error, 1)
	go func() { done <- s.Ping(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Ping: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ping blocked behind an open write transaction")
	}
}

// The context is the probe's deadline. A Ping that ignores it would keep the
// prober's goroutine alive past the round it belongs to.
func TestBoltStorage_PingHonoursContext(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "ping-ctx.bolt"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Ping(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Ping with a cancelled context = %v, want context.Canceled", err)
	}
}
