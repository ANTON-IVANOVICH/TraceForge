package rules

import (
	"context"
	"sort"
	"time"

	"metrics-system/internal/server/storage"
)

// defaultLookback is how far back an instant selector may reach for a series'
// most recent sample. Without a bound, a rule would keep alerting on a metric
// whose agent died hours ago; with one, the series simply disappears from the
// vector and the alert resolves.
const defaultLookback = 5 * time.Minute

// StorageQuerier adapts the metric store to the expression evaluator's Querier.
//
// It is the enforcement point for multi-tenancy: when tenant is non-empty every
// query is rewritten to filter on `tenant=<tenant>`, overriding whatever the
// expression asked for. A rule owned by tenant-a is therefore structurally
// unable to observe tenant-b's series, no matter how its matchers are written.
type StorageQuerier struct {
	store    storage.Storage
	tenant   string
	lookback time.Duration
}

// NewStorageQuerier returns a querier scoped to tenant (empty = auth disabled,
// unrestricted).
func NewStorageQuerier(store storage.Storage, tenant string, lookback time.Duration) *StorageQuerier {
	if lookback <= 0 {
		lookback = defaultLookback
	}
	return &StorageQuerier{store: store, tenant: tenant, lookback: lookback}
}

// scoped copies the caller's equality matchers and forces the tenant filter.
func (q *StorageQuerier) scoped(matchers map[string]string) map[string]string {
	out := make(map[string]string, len(matchers)+1)
	for k, v := range matchers {
		out[k] = v
	}
	if q.tenant != "" {
		out["tenant"] = q.tenant
	}
	return out
}

// Instant returns the most recent sample of each matching series at or before at.
func (q *StorageQuerier) Instant(_ context.Context, name string, matchers map[string]string, at time.Time) (Vector, error) {
	metrics, err := q.store.Query(storage.Query{
		Name:   name,
		Labels: q.scoped(matchers),
		From:   at.Add(-q.lookback),
		To:     at,
	})
	if err != nil {
		return nil, err
	}

	type latest struct {
		labels map[string]string
		t      time.Time
		v      float64
	}
	bySeries := make(map[string]*latest)
	for _, m := range metrics {
		key := storage.SeriesKey(m.Name, m.Labels)
		cur, ok := bySeries[key]
		if !ok {
			bySeries[key] = &latest{labels: m.Labels, t: m.Timestamp, v: m.Value}
			continue
		}
		if m.Timestamp.After(cur.t) {
			cur.t, cur.v, cur.labels = m.Timestamp, m.Value, m.Labels
		}
	}

	out := make(Vector, 0, len(bySeries))
	for _, key := range sortedMapKeys(bySeries) {
		s := bySeries[key]
		out = append(out, Sample{Labels: s.labels, Value: s.v})
	}
	return out, nil
}

// Range returns every point of each matching series within [from, to], ordered
// by time — range functions such as rate() rely on that ordering.
func (q *StorageQuerier) Range(_ context.Context, name string, matchers map[string]string, from, to time.Time) ([]Series, error) {
	metrics, err := q.store.Query(storage.Query{
		Name:   name,
		Labels: q.scoped(matchers),
		From:   from,
		To:     to,
	})
	if err != nil {
		return nil, err
	}

	bySeries := make(map[string]*Series)
	for _, m := range metrics {
		key := storage.SeriesKey(m.Name, m.Labels)
		s, ok := bySeries[key]
		if !ok {
			s = &Series{Labels: m.Labels}
			bySeries[key] = s
		}
		s.Points = append(s.Points, Point{T: m.Timestamp, V: m.Value})
	}

	out := make([]Series, 0, len(bySeries))
	for _, key := range sortedMapKeys(bySeries) {
		s := bySeries[key]
		sort.Slice(s.Points, func(i, j int) bool { return s.Points[i].T.Before(s.Points[j].T) })
		out = append(out, *s)
	}
	return out, nil
}

// sortedMapKeys keeps vector ordering deterministic, which makes evaluation
// reproducible and tests stable.
func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
