package storage

import (
	"fmt"
	"sort"
	"time"

	"metrics-system/internal/model"
)

// Query describes a read against the store. Name is required.
type Query struct {
	Name       string
	Labels     map[string]string // label filter; nil = match all
	From, To   time.Time         // time window; zero = open bound
	Aggregator Aggregator        // nil = raw points
	Step       time.Duration     // aggregation window size
	Limit      int               // 0 = unlimited
}

// ApplyQuery converts one series' already-time-filtered points into result
// metrics: raw points when q.Aggregator is nil, otherwise one aggregated value
// per non-empty window of q.Step. It does not apply q.Limit — the caller applies
// the limit across all series. Every storage backend uses this so aggregation
// behaves identically regardless of where the points came from.
func ApplyQuery(q Query, name string, typ model.MetricType, labels map[string]string, points []Point) []model.Metric {
	if len(points) == 0 {
		return nil
	}
	if q.Aggregator == nil {
		out := make([]model.Metric, 0, len(points))
		for _, pt := range points {
			out = append(out, model.Metric{
				Name:      name,
				Type:      typ,
				Value:     pt.Value,
				Timestamp: pt.Timestamp,
				Labels:    CloneLabels(labels),
			})
		}
		return out
	}
	windows := splitWindows(points, q.From, q.To, q.Step)
	out := make([]model.Metric, 0, len(windows))
	for _, w := range windows {
		out = append(out, model.Metric{
			Name:      name,
			Type:      typ,
			Value:     q.Aggregator.Aggregate(w.points),
			Timestamp: w.end,
			Labels:    CloneLabels(labels),
		})
	}
	return out
}

// Aggregator collapses a window of points into a single value (strategy pattern).
type Aggregator interface {
	Name() string
	Aggregate(points []Point) float64
}

// AggregatorByName maps a query string to an aggregator. An empty string means
// "no aggregation" (raw points) and returns a nil aggregator without error.
func AggregatorByName(name string) (Aggregator, error) {
	switch name {
	case "", "none", "raw":
		return nil, nil
	case "avg", "mean":
		return avgAgg{}, nil
	case "min":
		return minAgg{}, nil
	case "max":
		return maxAgg{}, nil
	case "sum":
		return sumAgg{}, nil
	case "count":
		return countAgg{}, nil
	case "p50":
		return percentileAgg{p: 0.50}, nil
	case "p90":
		return percentileAgg{p: 0.90}, nil
	case "p95":
		return percentileAgg{p: 0.95}, nil
	case "p99":
		return percentileAgg{p: 0.99}, nil
	default:
		return nil, fmt.Errorf("unknown aggregator: %q", name)
	}
}

type avgAgg struct{}

func (avgAgg) Name() string { return "avg" }
func (avgAgg) Aggregate(p []Point) float64 {
	if len(p) == 0 {
		return 0
	}
	var sum float64
	for _, pt := range p {
		sum += pt.Value
	}
	return sum / float64(len(p))
}

type minAgg struct{}

func (minAgg) Name() string { return "min" }
func (minAgg) Aggregate(p []Point) float64 {
	if len(p) == 0 {
		return 0
	}
	m := p[0].Value
	for _, pt := range p[1:] {
		if pt.Value < m {
			m = pt.Value
		}
	}
	return m
}

type maxAgg struct{}

func (maxAgg) Name() string { return "max" }
func (maxAgg) Aggregate(p []Point) float64 {
	if len(p) == 0 {
		return 0
	}
	m := p[0].Value
	for _, pt := range p[1:] {
		if pt.Value > m {
			m = pt.Value
		}
	}
	return m
}

type sumAgg struct{}

func (sumAgg) Name() string { return "sum" }
func (sumAgg) Aggregate(p []Point) float64 {
	var sum float64
	for _, pt := range p {
		sum += pt.Value
	}
	return sum
}

type countAgg struct{}

func (countAgg) Name() string { return "count" }
func (countAgg) Aggregate(p []Point) float64 { return float64(len(p)) }

// percentileAgg computes the p-quantile via nearest-rank on the sorted values.
type percentileAgg struct{ p float64 }

func (a percentileAgg) Name() string { return fmt.Sprintf("p%g", a.p*100) }
func (a percentileAgg) Aggregate(p []Point) float64 {
	if len(p) == 0 {
		return 0
	}
	values := make([]float64, len(p))
	for i, pt := range p {
		values[i] = pt.Value
	}
	sort.Float64s(values)
	idx := int(float64(len(values)-1) * a.p)
	return values[idx]
}

// window is a bucket of points aggregated to a single output timestamp.
type window struct {
	end    time.Time
	points []Point
}

// splitWindows buckets time-filtered points into [from,to] windows of size step.
// Empty windows are skipped. A non-positive step yields one window over all points.
func splitWindows(points []Point, from, to time.Time, step time.Duration) []window {
	if len(points) == 0 {
		return nil
	}
	if step <= 0 {
		return []window{{end: points[len(points)-1].Timestamp, points: points}}
	}

	start := from
	if start.IsZero() {
		start = points[0].Timestamp
	}
	end := to
	if end.IsZero() {
		end = points[len(points)-1].Timestamp
	}

	var windows []window
	for winStart := start; !winStart.After(end); winStart = winStart.Add(step) {
		winEnd := winStart.Add(step)
		var bucket []Point
		for _, p := range points {
			if !p.Timestamp.Before(winStart) && p.Timestamp.Before(winEnd) {
				bucket = append(bucket, p)
			}
		}
		if len(bucket) > 0 {
			windows = append(windows, window{end: winEnd, points: bucket})
		}
	}
	return windows
}
