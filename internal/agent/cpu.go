package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"

	"metrics-system/internal/model"
)

type CPUCollector struct {
	hostname string
}

func NewCPUCollector(hostname string) *CPUCollector {
	return &CPUCollector{hostname: hostname}
}

func (c *CPUCollector) Name() string { return "cpu" }

func (c *CPUCollector) Collect(ctx context.Context) ([]model.Metric, error) {
	percents, err := cpu.PercentWithContext(ctx, 0, false)
	if err != nil {
		return nil, fmt.Errorf("collect cpu: %w", err)
	}
	if len(percents) == 0 {
		return nil, fmt.Errorf("collect cpu: empty result")
	}

	now := time.Now().UTC()
	return []model.Metric{{
		Name:      "cpu_usage_percent",
		Type:      model.MetricTypeGauge,
		Value:     percents[0],
		Timestamp: now,
		Labels: map[string]string{
			"host": c.hostname,
		},
	}}, nil
}
