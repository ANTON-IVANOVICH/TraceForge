package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/shirou/gopsutil/v4/host"

	"metrics-system/internal/model"
)

type UptimeCollector struct {
	hostname string
}

func NewUptimeCollector(hostname string) *UptimeCollector {
	return &UptimeCollector{hostname: hostname}
}

func (c *UptimeCollector) Name() string { return "uptime" }

func (c *UptimeCollector) Collect(ctx context.Context) ([]model.Metric, error) {
	uptimeSec, err := host.UptimeWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("collect uptime: %w", err)
	}

	return []model.Metric{{
		Name:      "uptime_seconds",
		Type:      model.MetricTypeCounter,
		Value:     float64(uptimeSec),
		Timestamp: time.Now().UTC(),
		Labels: map[string]string{
			"host": c.hostname,
		},
	}}, nil
}
