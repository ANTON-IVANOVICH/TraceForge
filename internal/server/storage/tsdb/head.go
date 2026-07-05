package tsdb

import (
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/server/storage"
)

// head is the in-memory chunk that receives current writes before they are
// flushed to disk as an immutable chunk. It is NOT internally synchronized —
// the owning TSDB guards it with its RWMutex.
type head struct {
	series           map[string]*storage.Series // canonical key -> series
	minTime, maxTime int64
	points           int64
}

func newHead() *head {
	return &head{series: make(map[string]*storage.Series)}
}

func (h *head) write(m model.Metric) {
	key := storage.SeriesKey(m.Name, m.Labels)
	s, ok := h.series[key]
	if !ok {
		s = &storage.Series{Name: m.Name, Type: m.Type, Labels: storage.CloneLabels(m.Labels)}
		h.series[key] = s
	}
	s.Points = append(s.Points, storage.Point{Timestamp: m.Timestamp, Value: m.Value})

	ns := m.Timestamp.UnixNano()
	if h.points == 0 {
		h.minTime, h.maxTime = ns, ns
	} else {
		if ns < h.minTime {
			h.minTime = ns
		}
		if ns > h.maxTime {
			h.maxTime = ns
		}
	}
	h.points++
}

func (h *head) isEmpty() bool { return h.points == 0 }

// shouldFlush reports whether the head has grown big enough (by point count) or
// wide enough (by time span) to be flushed to a chunk.
func (h *head) shouldFlush(maxAge time.Duration, maxPoints int64) bool {
	if h.points >= maxPoints {
		return true
	}
	if h.points > 0 && h.maxTime-h.minTime > maxAge.Nanoseconds() {
		return true
	}
	return false
}

// snapshot returns a deep copy of the head's series, safe to hand to chunk.Write.
func (h *head) snapshot() []storage.Series {
	out := make([]storage.Series, 0, len(h.series))
	for _, s := range h.series {
		out = append(out, storage.Series{
			Name:   s.Name,
			Type:   s.Type,
			Labels: storage.CloneLabels(s.Labels),
			Points: append([]storage.Point(nil), s.Points...),
		})
	}
	return out
}
