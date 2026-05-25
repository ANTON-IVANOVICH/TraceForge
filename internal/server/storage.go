package server

import (
	"sync"

	"metrics-system/internal/model"
)

type Storage struct {
	mu      sync.RWMutex
	metrics []model.Metric
}

func NewStorage() *Storage {
	return &Storage{metrics: make([]model.Metric, 0, 1024)}
}

func (s *Storage) Add(batch model.Batch) {
	s.mu.Lock()
	s.metrics = append(s.metrics, batch.Metrics...)
	s.mu.Unlock()
}

func (s *Storage) All() []model.Metric {
	s.mu.RLock()
	out := make([]model.Metric, len(s.metrics))
	copy(out, s.metrics)
	s.mu.RUnlock()
	return out
}

func (s *Storage) Count() int {
	s.mu.RLock()
	n := len(s.metrics)
	s.mu.RUnlock()
	return n
}
