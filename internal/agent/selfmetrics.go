package agent

import (
	"metrics-system/internal/buildinfo"
	"metrics-system/internal/promexport"
)

// SelfMetrics exposes the agent's own counters for a Prometheus scrape.
//
// The agent is a DaemonSet: nothing routes traffic to it, and when it stops
// working the symptom is an absence — a host whose series simply stop arriving.
// An absence is the hardest thing to alert on, which is why these counters exist
// on the agent itself rather than being inferred at the server.
//
// `traceforge_agent_collect_failures_total` is the one that matters. A collector
// that fails on every tick logs a warning and is skipped; the agent keeps sending
// whatever the others produced, so the batch still arrives, the server still
// answers 202, and nothing anywhere is red. The counter is what makes it red.
func (a *Agent) SelfMetrics() promexport.Gatherer {
	counter := func(name, help string, v uint64) promexport.Family {
		return promexport.Family{
			Name:    name,
			Help:    help,
			Type:    promexport.TypeCounter,
			Samples: []promexport.Sample{{Value: float64(v)}},
		}
	}
	return promexport.GathererFunc(func() []promexport.Family {
		s := a.Stats()
		families := []promexport.Family{
			counter("traceforge_agent_ticks_total", "Collection ticks started.", s.Ticks),
			counter("traceforge_agent_collected_metrics_total", "Metrics produced by the collectors.", s.Collected),
			counter("traceforge_agent_collect_failures_total", "Collector invocations that returned an error.", s.CollectFailures),
			counter("traceforge_agent_batches_sent_total", "Batches accepted by the server.", s.BatchesSent),
			counter("traceforge_agent_send_failures_total", "Batches the transport failed to deliver.", s.SendFailures),
		}
		// Absent, rather than zero, until the first successful collection. A zero
		// here would read as "last collected at the epoch", and every staleness
		// alert written against it would fire on a freshly started agent.
		if !s.LastCollect.IsZero() {
			families = append(families, promexport.Family{
				Name:    "traceforge_agent_last_collect_timestamp_seconds",
				Help:    "Unix time of the last tick that produced at least one metric.",
				Type:    promexport.TypeGauge,
				Samples: []promexport.Sample{{Value: float64(s.LastCollect.UnixNano()) / 1e9}},
			})
		}
		return families
	})
}

// BuildInfoGatherer exposes the agent's build as a constant-1 gauge, so that
// `count by (version) (traceforge_agent_build_info)` reports how far a DaemonSet
// rollout has reached across the fleet.
//
// It is a different metric name from the server's traceforge_build_info on
// purpose. The two are scraped into the same Prometheus, and one metric name
// carrying two different processes' versions makes the rollout query a join.
func BuildInfoGatherer() promexport.Gatherer {
	info := buildinfo.Get()
	dirty := "false"
	if info.Dirty {
		dirty = "true"
	}
	family := promexport.Family{
		Name: "traceforge_agent_build_info",
		Help: "Always 1. The agent build's identity is in the labels.",
		Type: promexport.TypeGauge,
		Samples: []promexport.Sample{{
			Labels: []promexport.Label{
				{Name: "version", Value: info.Version},
				{Name: "commit", Value: info.Commit},
				{Name: "dirty", Value: dirty},
				{Name: "go_version", Value: info.GoVersion},
				{Name: "platform", Value: info.Platform},
			},
			Value: 1,
		}},
	}
	return promexport.GathererFunc(func() []promexport.Family {
		return []promexport.Family{family}
	})
}
