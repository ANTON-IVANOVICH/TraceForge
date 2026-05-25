package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/shirou/gopsutil/v4/disk"

	"metrics-system/internal/model"
)

type DiskCollector struct {
	hostname string
	path     string
}

func NewDiskCollector(hostname, path string) *DiskCollector {
	if path == "" {
		path = "/"
	}
	return &DiskCollector{hostname: hostname, path: path}
}

func (c *DiskCollector) Name() string { return "disk" }

func (c *DiskCollector) Collect(ctx context.Context) ([]model.Metric, error) {
	usage, err := disk.UsageWithContext(ctx, c.path)
	if err != nil {
		return nil, fmt.Errorf("collect disk usage: %w", err)
	}

	now := time.Now().UTC()
	return []model.Metric{
		{
			Name:      "disk_total_bytes",
			Type:      model.MetricTypeGauge,
			Value:     float64(usage.Total),
			Timestamp: now,
			Labels: map[string]string{
				"host": c.hostname,
				"path": c.path,
			},
		},
		{
			Name:      "disk_used_bytes",
			Type:      model.MetricTypeGauge,
			Value:     float64(usage.Used),
			Timestamp: now,
			Labels: map[string]string{
				"host": c.hostname,
				"path": c.path,
			},
		},
		{
			Name:      "disk_used_percent",
			Type:      model.MetricTypeGauge,
			Value:     usage.UsedPercent,
			Timestamp: now,
			Labels: map[string]string{
				"host": c.hostname,
				"path": c.path,
			},
		},
	}, nil
}
