package rules

import (
	"context"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
)

func storeWith(t *testing.T, metrics ...model.Metric) storage.Storage {
	t.Helper()
	s := storage.NewMemoryStorage()
	if err := s.WriteBatch(metrics); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func metric(name string, v float64, at time.Time, labels map[string]string) model.Metric {
	return model.Metric{Name: name, Type: model.MetricTypeGauge, Value: v, Timestamp: at, Labels: labels}
}

// The querier is the enforcement point for multi-tenancy: a tenant-scoped rule
// must be structurally unable to observe another tenant's series, even when its
// expression explicitly asks for one.
func TestStorageQuerierForcesTenantScope(t *testing.T) {
	t.Parallel()
	store := storeWith(t,
		metric("cpu", 95, evalAt, lbl("tenant", "tenant-a", "host", "a")),
		metric("cpu", 99, evalAt, lbl("tenant", "tenant-b", "host", "b")),
	)

	q := NewStorageQuerier(store, "tenant-a", time.Minute)

	// Even an explicit tenant-b matcher is overridden.
	got, err := q.Instant(context.Background(), "cpu", map[string]string{"tenant": "tenant-b"}, evalAt)
	if err != nil {
		t.Fatalf("Instant: %v", err)
	}
	if len(got) != 1 || got[0].Labels["tenant"] != "tenant-a" {
		t.Fatalf("tenant-a querier saw %s — isolation breach", render(got))
	}

	// The full expression path must be scoped too.
	expr, _, err := Parse(`cpu{tenant="tenant-b"} > 90`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	vec, err := expr.Eval(context.Background(), q, evalAt)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	// The forced tenant-a filter wins at the store, but the expression's own
	// in-memory tenant="tenant-b" equality matcher was pushed down and replaced,
	// so what comes back is tenant-a's series only.
	if len(vec) != 1 || vec[0].Labels["tenant"] != "tenant-a" {
		t.Fatalf("expression saw %s — isolation breach", render(vec))
	}
}

// An empty tenant means auth is disabled; the querier then sees everything.
func TestStorageQuerierUnscopedSeesAll(t *testing.T) {
	t.Parallel()
	store := storeWith(t,
		metric("cpu", 95, evalAt, lbl("host", "a")),
		metric("cpu", 99, evalAt, lbl("host", "b")),
	)
	q := NewStorageQuerier(store, "", 0)

	got, err := q.Instant(context.Background(), "cpu", nil, evalAt)
	if err != nil {
		t.Fatalf("Instant: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d series, want 2", len(got))
	}
}

// Instant returns the newest point of each series, not an arbitrary one.
func TestStorageQuerierInstantTakesLatestPoint(t *testing.T) {
	t.Parallel()
	store := storeWith(t,
		metric("cpu", 10, evalAt.Add(-2*time.Minute), lbl("host", "a")),
		metric("cpu", 50, evalAt.Add(-time.Minute), lbl("host", "a")),
		metric("cpu", 30, evalAt.Add(-3*time.Minute), lbl("host", "a")),
	)
	q := NewStorageQuerier(store, "", 10*time.Minute)

	got, err := q.Instant(context.Background(), "cpu", nil, evalAt)
	if err != nil {
		t.Fatalf("Instant: %v", err)
	}
	if len(got) != 1 || got[0].Value != 50 {
		t.Fatalf("got %s, want the newest value 50", render(got))
	}
}

// A series whose newest point predates the lookback window disappears from the
// vector, which is what lets an alert on a dead agent resolve itself.
func TestStorageQuerierRespectsLookback(t *testing.T) {
	t.Parallel()
	store := storeWith(t, metric("cpu", 95, evalAt.Add(-time.Hour), lbl("host", "a")))
	q := NewStorageQuerier(store, "", 5*time.Minute)

	got, err := q.Instant(context.Background(), "cpu", nil, evalAt)
	if err != nil {
		t.Fatalf("Instant: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("stale series survived the lookback window: %s", render(got))
	}
}

// Range functions depend on points being time-ordered.
func TestStorageQuerierRangeSortsPoints(t *testing.T) {
	t.Parallel()
	store := storeWith(t,
		metric("cpu", 30, evalAt.Add(-1*time.Minute), lbl("host", "a")),
		metric("cpu", 10, evalAt.Add(-3*time.Minute), lbl("host", "a")),
		metric("cpu", 20, evalAt.Add(-2*time.Minute), lbl("host", "a")),
	)
	q := NewStorageQuerier(store, "", 0)

	series, err := q.Range(context.Background(), "cpu", nil, evalAt.Add(-10*time.Minute), evalAt)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("got %d series, want 1", len(series))
	}
	want := []float64{10, 20, 30}
	for i, p := range series[0].Points {
		if p.V != want[i] {
			t.Fatalf("points = %v, want ascending by time %v", series[0].Points, want)
		}
	}
}
