package storage

import (
	"errors"
	"sync"

	"metrics-system/internal/model"
)

// MemoryStorage keeps every series in memory, guarded by an RWMutex, with a
// name index for fast lookups. It implements Storage.
type MemoryStorage struct {
	mu     sync.RWMutex
	series map[string]*Series // canonical key -> series (write dedup)
	idx    *index             // name -> series (query lookup)
}

// NewMemoryStorage returns an empty in-memory store.
func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{
		series: make(map[string]*Series),
		idx:    newIndex(),
	}
}

// Write appends the metric's value to its series, creating the series on first
// sight. Safe for concurrent use.
func (s *MemoryStorage) Write(m model.Metric) {
	key := seriesKey(m.Name, m.Labels)

	s.mu.Lock()
	defer s.mu.Unlock()

	series, ok := s.series[key]
	if !ok {
		series = &Series{
			Name:   m.Name,
			Type:   m.Type,
			Labels: cloneLabels(m.Labels),
			Points: make([]Point, 0, 64),
		}
		s.series[key] = series
		s.idx.add(key, series)
	}
	series.Points = append(series.Points, Point{Timestamp: m.Timestamp, Value: m.Value})
}

// Query returns metrics for the named series, optionally filtered by labels and
// time and optionally aggregated into windows of q.Step.
func (s *MemoryStorage) Query(q Query) ([]model.Metric, error) {
	if q.Name == "" {
		return nil, errors.New("query name is required")
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []model.Metric
	for _, series := range s.idx.seriesForName(q.Name) {
		if !matchLabels(series.Labels, q.Labels) {
			continue
		}
		points := filterTime(series.Points, q.From, q.To)
		if len(points) == 0 {
			continue
		}

		if q.Aggregator == nil {
			for _, pt := range points {
				result = append(result, model.Metric{
					Name:      series.Name,
					Type:      series.Type,
					Value:     pt.Value,
					Timestamp: pt.Timestamp,
					Labels:    cloneLabels(series.Labels),
				})
				if q.Limit > 0 && len(result) >= q.Limit {
					return result, nil
				}
			}
			continue
		}

		for _, w := range splitWindows(points, q.From, q.To, q.Step) {
			result = append(result, model.Metric{
				Name:      series.Name,
				Type:      series.Type,
				Value:     q.Aggregator.Aggregate(w.points),
				Timestamp: w.end,
				Labels:    cloneLabels(series.Labels),
			})
			if q.Limit > 0 && len(result) >= q.Limit {
				return result, nil
			}
		}
	}
	return result, nil
}

// Stats reports the number of series and total points currently stored.
func (s *MemoryStorage) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var points int64
	for _, series := range s.series {
		points += int64(len(series.Points))
	}
	return Stats{Series: len(s.series), Points: points}
}
