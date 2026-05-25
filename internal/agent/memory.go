package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/shirou/gopsutil/v4/mem"

	"metrics-system/internal/model"
)

type MemoryCollector struct {
	hostname string
}

func NewMemoryCollector(hostname string) *MemoryCollector {
	return &MemoryCollector{hostname: hostname}
}

func (c *MemoryCollector) Name() string { return "memory" }

func (c *MemoryCollector) Collect(ctx context.Context) ([]model.Metric, error) {
	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("collect memory: %w", err)
	}

	now := time.Now().UTC()
	labels := map[string]string{"host": c.hostname}
	return []model.Metric{
		{
			Name:      "memory_total_bytes",
			Type:      model.MetricTypeGauge,
			Value:     float64(vm.Total),
			Timestamp: now,
			Labels:    labels,
		},
		{
			Name:      "memory_used_bytes",
			Type:      model.MetricTypeGauge,
			Value:     float64(vm.Used),
			Timestamp: now,
			Labels:    labels,
		},
		{
			Name:      "memory_used_percent",
			Type:      model.MetricTypeGauge,
			Value:     vm.UsedPercent,
			Timestamp: now,
			Labels:    labels,
		},
	}, nil
}
