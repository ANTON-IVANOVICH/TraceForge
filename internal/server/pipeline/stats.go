package pipeline

import "sync/atomic"

// Stats holds pipeline counters as typed atomics so the hot path never takes a
// lock. Zero value is ready to use.
type Stats struct {
	ingested atomic.Int64
	dropped  atomic.Int64
	invalid  atomic.Int64
	stored   atomic.Int64
}

// NewStats returns a zeroed Stats.
func NewStats() *Stats { return &Stats{} }

func (s *Stats) IncIngested(n int64) { s.ingested.Add(n) }
func (s *Stats) IncDropped(n int64)  { s.dropped.Add(n) }
func (s *Stats) IncInvalid(n int64)  { s.invalid.Add(n) }
func (s *Stats) IncStored(n int64)   { s.stored.Add(n) }

// Snapshot is an immutable point-in-time view of the counters.
type Snapshot struct {
	Ingested int64 `json:"ingested"`
	Dropped  int64 `json:"dropped"`
	Invalid  int64 `json:"invalid"`
	Stored   int64 `json:"stored"`
}

// Snapshot reads all counters. The reads are individually atomic but not a
// single consistent transaction — fine for observability.
func (s *Stats) Snapshot() Snapshot {
	return Snapshot{
		Ingested: s.ingested.Load(),
		Dropped:  s.dropped.Load(),
		Invalid:  s.invalid.Load(),
		Stored:   s.stored.Load(),
	}
}
