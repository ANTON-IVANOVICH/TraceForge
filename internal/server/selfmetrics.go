package server

import (
	"metrics-system/internal/alerting"
	"metrics-system/internal/buildinfo"
	"metrics-system/internal/promexport"
	"metrics-system/internal/server/pipeline"
	"metrics-system/internal/server/ratelimit"
	"metrics-system/internal/server/storage"
)

// The gatherers below expose TraceForge's own state in the Prometheus text
// format, so the thing that watches everything else can itself be watched. Every
// name is prefixed `traceforge_`, including the Go runtime ones.
//
// That last choice is deliberate and costs a little familiarity. Emitting
// `go_goroutines` and `go_memstats_alloc_bytes` — the names prometheus's own
// client library uses — would make community dashboards work out of the box, and
// would be a lie the moment our definition drifted from theirs by a byte. A
// dashboard panel that silently means something else is worse than one that shows
// no data.

// BuildInfoGatherer exposes the running build as a constant-1 gauge with the
// version in its labels.
//
// The value carries no information; the labels do. This is the standard trick for
// exporting a string through a system that only stores float64s, and it is what
// makes `count by (version) (traceforge_build_info)` answer "how far along is the
// rollout?" during a deploy.
func BuildInfoGatherer() promexport.Gatherer {
	info := buildinfo.Get()
	dirty := "false"
	if info.Dirty {
		dirty = "true"
	}
	family := promexport.Family{
		Name: "traceforge_build_info",
		Help: "Always 1. The build's identity is in the labels.",
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
	// The build cannot change while the process runs, so the family is built once
	// and handed out by reference on every scrape.
	return promexport.GathererFunc(func() []promexport.Family {
		return []promexport.Family{family}
	})
}

// PipelineGatherer exposes the ingest pipeline's counters.
//
// The identity `ingested == stored + invalid + failed` holds once the pipeline is
// drained, and `offered == ingested + dropped` holds always. Both are worth
// alerting on: a gap in the first means metrics vanished between the door and the
// disk.
func PipelineGatherer(p *pipeline.Pipeline) promexport.Gatherer {
	counter := func(name, help string, v int64) promexport.Family {
		return promexport.Family{
			Name:    name,
			Help:    help,
			Type:    promexport.TypeCounter,
			Samples: []promexport.Sample{{Value: float64(v)}},
		}
	}
	return promexport.GathererFunc(func() []promexport.Family {
		s := p.Stats()
		return []promexport.Family{
			counter("traceforge_pipeline_ingested_total", "Metrics accepted into the pipeline.", s.Ingested),
			counter("traceforge_pipeline_dropped_total", "Metrics refused at the door because the pipeline was full. The caller was told.", s.Dropped),
			counter("traceforge_pipeline_invalid_total", "Metrics accepted and then rejected by validation.", s.Invalid),
			counter("traceforge_pipeline_failed_total", "Metrics accepted, valid, and lost to a storage write error. The caller was told 202.", s.Failed),
			counter("traceforge_pipeline_stored_total", "Metrics handed to storage.", s.Stored),
		}
	})
}

// StorageGatherer exposes how much data the store holds.
func StorageGatherer(s storage.Storage) promexport.Gatherer {
	return promexport.GathererFunc(func() []promexport.Family {
		st := s.Stats()
		return []promexport.Family{
			{
				Name:    "traceforge_storage_series",
				Help:    "Distinct series currently stored.",
				Type:    promexport.TypeGauge,
				Samples: []promexport.Sample{{Value: float64(st.Series)}},
			},
			{
				Name:    "traceforge_storage_points",
				Help:    "Data points currently stored.",
				Type:    promexport.TypeGauge,
				Samples: []promexport.Sample{{Value: float64(st.Points)}},
			},
		}
	})
}

// RateLimitGatherer exposes the number of live token buckets.
//
// It is a memory gauge as much as a traffic one: the limiter keys buckets by
// agent id, and a client that invents a new id per request would grow this
// number until the sweeper's cap stopped it. Watching it is how that is noticed.
func RateLimitGatherer(l *ratelimit.PerAgentLimiter) promexport.Gatherer {
	return promexport.GathererFunc(func() []promexport.Family {
		return []promexport.Family{{
			Name:    "traceforge_ratelimit_buckets",
			Help:    "Per-agent token buckets currently held by the rate limiter.",
			Type:    promexport.TypeGauge,
			Samples: []promexport.Sample{{Value: float64(l.Len())}},
		}}
	})
}

// AlertingGatherer exposes the alerts this server currently holds open.
//
// Severity comes from the alert's labels rather than a field, because that is
// where the rule puts it; an alert whose rule omitted it is labelled "unknown"
// rather than dropped, so a rule missing its severity shows up in the dashboard
// instead of disappearing from it.
func AlertingGatherer(svc *alerting.Service) promexport.Gatherer {
	return promexport.GathererFunc(func() []promexport.Family {
		// The empty tenant means every tenant. This endpoint is not tenant-scoped
		// and carries no tenant label: it is an operator's view of the process, and
		// exposing which tenants exist on an unauthenticated port would be a leak.
		active := svc.ActiveAlerts("")

		type key struct{ state, severity string }
		counts := make(map[key]int, 8)
		for _, st := range active {
			severity := st.Labels["severity"]
			if severity == "" {
				severity = "unknown"
			}
			counts[key{state: string(st.State), severity: severity}]++
		}

		samples := make([]promexport.Sample, 0, len(counts))
		for k, n := range counts {
			samples = append(samples, promexport.Sample{
				Labels: []promexport.Label{
					{Name: "state", Value: k.state},
					{Name: "severity", Value: k.severity},
				},
				Value: float64(n),
			})
		}
		return []promexport.Family{{
			Name:    "traceforge_alerting_active_alerts",
			Help:    "Alerts currently pending or firing, by state and severity.",
			Type:    promexport.TypeGauge,
			Samples: samples,
		}}
	})
}
