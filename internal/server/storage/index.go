package storage

import (
	"sort"
	"strings"
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

// seriesKey canonicalizes a metric into a stable key. Labels are sorted so
// {host=a,region=b} and {region=b,host=a} collapse to the same series.
func seriesKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	b.WriteByte('}')
	return b.String()
}

func cloneLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// matchLabels reports whether a series' labels satisfy every filter label.
// An empty filter matches everything.
func matchLabels(labels, filter map[string]string) bool {
	for k, v := range filter {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// filterTime returns points within [from, to]. Zero bounds are treated as
// open. It scans linearly so it stays correct even if points are not perfectly
// time-ordered.
func filterTime(points []Point, from, to time.Time) []Point {
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
