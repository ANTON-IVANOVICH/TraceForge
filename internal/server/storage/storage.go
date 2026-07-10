// Package storage defines the time-series store abstraction the pipeline writes
// into and the query layer reads from, plus an in-memory implementation. The
// Storage interface is deliberately small so persistent backends (bbolt, a
// custom on-disk TSDB) can be swapped in without touching the pipeline or HTTP
// layers. Shared helpers (SeriesKey, MatchLabels, FilterTime, ApplyQuery) are
// exported for those backends to reuse.
package storage

import (
	"context"
	"time"

	"metrics-system/internal/model"
)

// Storage is the persistence boundary for the pipeline. All backends
// (memory, bolt, tsdb) implement it, so the pipeline is agnostic to which one
// is in use.
type Storage interface {
	// Write persists a single metric.
	Write(m model.Metric) error
	// WriteBatch persists many metrics at once (backends may commit them in one
	// transaction — much faster than one Write per point).
	WriteBatch(metrics []model.Metric) error
	// Query returns metrics matching q (raw points or aggregated windows).
	Query(q Query) ([]model.Metric, error)
	// Stats reports the current size of the store.
	Stats() Stats
	// Ping reports whether the store can still do its job. It answers the
	// readiness probe, so it carries two obligations the other methods do not.
	//
	// It must be cheap and it must not block behind a write: the probe runs every
	// few seconds on every replica, and a Ping that waits for the store's lock
	// turns one slow disk into a fleet-wide readiness failure — every replica
	// leaves the load balancer at the same moment, which is an outage caused by
	// the thing that was meant to detect one.
	//
	// It must report durability, not liveness. A backend that is accepting writes
	// into memory while its fsync fails is the most dangerous state this system
	// has: nothing is broken from the caller's side, and nothing is being kept.
	Ping(ctx context.Context) error
	// Close flushes and releases the backend's resources.
	Close() error
}

// Point is a single timestamped value inside a series.
type Point struct {
	Timestamp time.Time
	Value     float64
}

// Series is the set of points sharing one name + label set — the same unit of
// storage that Prometheus/VictoriaMetrics/InfluxDB use internally.
type Series struct {
	Name   string
	Type   model.MetricType
	Labels map[string]string
	Points []Point
}

// Stats is a snapshot of how much data the store currently holds.
type Stats struct {
	Series int   `json:"series"`
	Points int64 `json:"points"`
}
