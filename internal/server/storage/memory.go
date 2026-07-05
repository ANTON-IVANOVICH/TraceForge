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

// Write appends the metric's value to its series. Safe for concurrent use.
func (s *MemoryStorage) Write(m model.Metric) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeLocked(m)
	return nil
}

// WriteBatch appends many metrics under a single lock acquisition.
func (s *MemoryStorage) WriteBatch(metrics []model.Metric) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range metrics {
		s.writeLocked(m)
	}
	return nil
}

func (s *MemoryStorage) writeLocked(m model.Metric) {
	key := SeriesKey(m.Name, m.Labels)
	series, ok := s.series[key]
	if !ok {
		series = &Series{
			Name:   m.Name,
			Type:   m.Type,
			Labels: CloneLabels(m.Labels),
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
		if !MatchLabels(series.Labels, q.Labels) {
			continue
		}
		points := FilterTime(series.Points, q.From, q.To)
		for _, m := range ApplyQuery(q, series.Name, series.Type, series.Labels, points) {
			result = append(result, m)
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

// Close is a no-op for the in-memory store.
func (s *MemoryStorage) Close() error { return nil }
