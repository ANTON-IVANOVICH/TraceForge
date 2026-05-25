package agent

import (
	"context"

	"metrics-system/internal/model"
)

// Collector is any metric source that can collect one or more metrics.
type Collector interface {
	Name() string
	Collect(ctx context.Context) ([]model.Metric, error)
}
