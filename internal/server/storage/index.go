package storage

import (
	"time"
)

// index maps a metric name to the series carrying that name, so a query by
// name touches only the relevant series instead of scanning the whole store.
type index struct {
	byName map[string]map[string]*Series // name -> seriesKey -> series
}

func newIndex() *index {
	return &index{byName: make(map[string]map[string]*Series)}
}

func (ix *index) add(key string, s *Series) {
	bucket := ix.byName[s.Name]
	if bucket == nil {
		bucket = make(map[string]*Series)
		ix.byName[s.Name] = bucket
	}
	bucket[key] = s
}

func (ix *index) seriesForName(name string) []*Series {
	bucket := ix.byName[name]
	if len(bucket) == 0 {
		return nil
	}
	out := make([]*Series, 0, len(bucket))
	for _, s := range bucket {
		out = append(out, s)
	}
	return out
}

// CloneLabels returns a copy of in (nil for an empty map) so callers can't
// mutate a stored series' labels.
func CloneLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// MatchLabels reports whether a series' labels satisfy every filter label.
// An empty filter matches everything.
func MatchLabels(labels, filter map[string]string) bool {
	for k, v := range filter {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// FilterTime returns points within [from, to]. Zero bounds are treated as open.
// It scans linearly so it stays correct even if points are not perfectly
// time-ordered.
func FilterTime(points []Point, from, to time.Time) []Point {
	if from.IsZero() && to.IsZero() {
		return points
	}
	out := make([]Point, 0, len(points))
	for _, p := range points {
		if !from.IsZero() && p.Timestamp.Before(from) {
			continue
		}
		if !to.IsZero() && p.Timestamp.After(to) {
			continue
		}
		out = append(out, p)
	}
	return out
}
