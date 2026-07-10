package telemetry

import (
	"runtime/debug"
	"runtime/metrics"

	"metrics-system/internal/promexport"
)

// runtimeSamples are the runtime/metrics series worth a scrape, and what to call
// them on the wire.
var runtimeSamples = []struct {
	runtime string
	name    string
	help    string
	typ     promexport.Type
}{
	{"/sched/goroutines:goroutines", "traceforge_go_goroutines", "Goroutines that currently exist.", promexport.TypeGauge},
	{"/sched/gomaxprocs:threads", "traceforge_go_maxprocs", "The current GOMAXPROCS. On Linux the runtime derives it from the cgroup CPU quota and updates it as the quota changes.", promexport.TypeGauge},
	{"/memory/classes/heap/objects:bytes", "traceforge_go_heap_objects_bytes", "Memory occupied by live objects and dead objects not yet swept.", promexport.TypeGauge},
	{"/memory/classes/total:bytes", "traceforge_go_memory_total_bytes", "All memory mapped by the Go runtime.", promexport.TypeGauge},
	{"/gc/heap/allocs:bytes", "traceforge_go_heap_allocs_bytes_total", "Cumulative bytes allocated in heap objects.", promexport.TypeCounter},
	{"/gc/cycles/total:gc-cycles", "traceforge_go_gc_cycles_total", "Completed GC cycles.", promexport.TypeCounter},
}

// RuntimeGatherer exposes the Go runtime's own counters.
//
// It reads them through runtime/metrics rather than runtime.ReadMemStats.
// ReadMemStats stops the world for the duration of the read; on a fifteen-second
// scrape from three Prometheus replicas that is three stop-the-world pauses a
// scrape interval, paid by every request in flight. runtime/metrics samples from
// the same underlying counters without the pause.
//
// GOMAXPROCS is read from /sched/gomaxprocs:threads and not from
// runtime.GOMAXPROCS(0). The zero argument is in fact a safe read — the runtime
// returns before it marks the value custom — but the neighbouring call
// runtime.GOMAXPROCS(runtime.NumCPU()), which looks like an equally harmless
// no-op, sets sched.customGOMAXPROCS and permanently disables the runtime's
// cgroup-quota tracking. Reading a metric cannot make that mistake.
func RuntimeGatherer() promexport.Gatherer {
	return promexport.GathererFunc(func() []promexport.Family {
		samples := make([]metrics.Sample, len(runtimeSamples))
		for i, rs := range runtimeSamples {
			samples[i].Name = rs.runtime
		}
		metrics.Read(samples)

		families := make([]promexport.Family, 0, len(runtimeSamples)+1)
		for i, rs := range runtimeSamples {
			// A metric the runtime does not know is reported as KindBad rather than
			// as an error. Skipping it keeps a future Go release that renames a
			// counter from blanking the whole scrape.
			if samples[i].Value.Kind() != metrics.KindUint64 {
				continue
			}
			families = append(families, promexport.Family{
				Name:    rs.name,
				Help:    rs.help,
				Type:    rs.typ,
				Samples: []promexport.Sample{{Value: float64(samples[i].Value.Uint64())}},
			})
		}

		// SetMemoryLimit(-1) is the documented way to read the current soft memory
		// limit without changing it. Exposing it turns "why is the GC running so
		// often" into a question a dashboard answers.
		families = append(families, promexport.Family{
			Name:    "traceforge_go_memory_limit_bytes",
			Help:    "The soft memory limit (GOMEMLIMIT). math.MaxInt64 means unlimited.",
			Type:    promexport.TypeGauge,
			Samples: []promexport.Sample{{Value: float64(debug.SetMemoryLimit(-1))}},
		})
		return families
	})
}
