// Package storage holds the in-memory time-series store that the pipeline
// writes into and the query layer reads from. The Storage interface is kept
// deliberately small so a persistent implementation can replace MemoryStorage
// in a later stage without touching the pipeline or HTTP layers.
package storage

import (
	"time"

	"metrics-system/internal/model"
)

// Storage is the persistence boundary for the pipeline.
type Storage interface {
	// Write appends a single metric to its series.
	Write(m model.Metric)
	// Query returns metrics matching q (raw points or aggregated windows).
	Query(q Query) ([]model.Metric, error)
	// Stats reports the current size of the store.
	Stats() Stats
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
