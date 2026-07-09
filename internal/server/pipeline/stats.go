package pipeline

import "sync/atomic"

// Stats holds pipeline counters as typed atomics so the hot path never takes a
// lock. Zero value is ready to use.
//
// The counters mean precisely this, and the distinction matters to whoever is
// paged at 3am:
//
//   - ingested — metrics accepted into the pipeline.
//   - dropped  — metrics refused at the door because the ingest buffer was full.
//     It counts *rejected attempts*, not metrics ultimately lost: an agent that
//     retries after an HTTP 503 adds to it each time, and may still have every
//     metric stored. Rising `dropped` means "the server is shedding load", not
//     "data is gone".
//   - invalid  — metrics accepted, then rejected by validation. Lost, on purpose.
//   - failed   — metrics accepted and valid, whose write to storage errored.
//     Lost, not on purpose. This is the one to alert on.
//   - stored   — metrics durably handed to the storage backend.
//
// Once the pipeline has drained, everything accepted has reached exactly one
// terminal state:
//
//	ingested == stored + invalid + failed
//
// Note that `dropped` is not in that identity — a dropped metric was never
// ingested. Its own identity is `offered == ingested + dropped`, per attempt.
type Stats struct {
	ingested atomic.Int64
	dropped  atomic.Int64
	invalid  atomic.Int64
	failed   atomic.Int64
	stored   atomic.Int64
}

// NewStats returns a zeroed Stats.
func NewStats() *Stats { return &Stats{} }

func (s *Stats) IncIngested(n int64) { s.ingested.Add(n) }
func (s *Stats) IncDropped(n int64)  { s.dropped.Add(n) }
func (s *Stats) IncInvalid(n int64)  { s.invalid.Add(n) }
func (s *Stats) IncFailed(n int64)   { s.failed.Add(n) }
func (s *Stats) IncStored(n int64)   { s.stored.Add(n) }

// Snapshot is an immutable point-in-time view of the counters.
type Snapshot struct {
	Ingested int64 `json:"ingested"`
	Dropped  int64 `json:"dropped"`
	Invalid  int64 `json:"invalid"`
	Failed   int64 `json:"failed"`
	Stored   int64 `json:"stored"`
}

// Snapshot reads all counters. The reads are individually atomic but not a
// single consistent transaction — fine for observability.
func (s *Stats) Snapshot() Snapshot {
	return Snapshot{
		Ingested: s.ingested.Load(),
		Dropped:  s.dropped.Load(),
		Invalid:  s.invalid.Load(),
		Failed:   s.failed.Load(),
		Stored:   s.stored.Load(),
	}
}
