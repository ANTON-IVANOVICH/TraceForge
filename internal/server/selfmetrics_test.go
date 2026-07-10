package server

import (
	"strings"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/promexport"
	"metrics-system/internal/server/pipeline"
	"metrics-system/internal/server/ratelimit"
	"metrics-system/internal/server/storage"
)

// families flattens a gatherer into name -> family for assertions.
func families(t *testing.T, g promexport.Gatherer) map[string]promexport.Family {
	t.Helper()
	out := make(map[string]promexport.Family)
	for _, f := range g.Gather() {
		if err := f.Validate(); err != nil {
			t.Errorf("family %s does not validate, so it would be dropped from every scrape: %v", f.Name, err)
		}
		if _, dup := out[f.Name]; dup {
			t.Errorf("family %s gathered twice; a scraper rejects a second HELP line", f.Name)
		}
		out[f.Name] = f
	}
	return out
}

func scalar(t *testing.T, f promexport.Family) float64 {
	t.Helper()
	if len(f.Samples) != 1 {
		t.Fatalf("family %s has %d samples, want 1", f.Name, len(f.Samples))
	}
	return f.Samples[0].Value
}

// TestBuildInfoGatherer. The value is always 1; the labels carry everything. That
// is what makes `count by (version) (traceforge_build_info)` report how far a
// rollout has reached.
func TestBuildInfoGatherer(t *testing.T) {
	fams := families(t, BuildInfoGatherer())
	f, ok := fams["traceforge_build_info"]
	if !ok {
		t.Fatal("traceforge_build_info is absent")
	}
	if got := scalar(t, f); got != 1 {
		t.Errorf("value = %v, want 1", got)
	}

	labels := make(map[string]string)
	for _, l := range f.Samples[0].Labels {
		labels[l.Name] = l.Value
	}
	for _, want := range []string{"version", "commit", "dirty", "go_version", "platform"} {
		if labels[want] == "" {
			t.Errorf("label %q is empty; the rollout is not observable without it", want)
		}
	}
	if !strings.HasPrefix(labels["go_version"], "go1.") {
		t.Errorf("go_version = %q", labels["go_version"])
	}
	if labels["dirty"] != "true" && labels["dirty"] != "false" {
		t.Errorf("dirty = %q, want a boolean an operator can filter on", labels["dirty"])
	}
}

// TestPipelineGathererReportsTheAccountingIdentity.
//
// `ingested == stored + invalid + failed` once drained. This is the promise that
// nothing vanishes between the door and the disk, and the gatherer is where an
// operator finally sees it.
func TestPipelineGathererReportsTheAccountingIdentity(t *testing.T) {
	store := storage.NewMemoryStorage()
	pipe := pipeline.New(store, pipeline.Config{
		IngestBuffer: 64, ValidateWorkers: 1, EnrichWorkers: 1, StoreWorkers: 1,
	}, testLogger())
	pipe.Start()

	now := time.Now().UTC()
	batch := model.Batch{AgentID: "a", Metrics: []model.Metric{
		{Name: "ok_one", Type: model.MetricTypeGauge, Value: 1, Timestamp: now},
		{Name: "ok_two", Type: model.MetricTypeGauge, Value: 2, Timestamp: now},
		{Name: "", Type: model.MetricTypeGauge, Value: 3, Timestamp: now}, // invalid: no name
	}}
	if !pipe.Ingest(batch) {
		t.Fatal("Ingest refused a batch the buffer had room for")
	}
	pipe.Shutdown()

	fams := families(t, PipelineGatherer(pipe))
	get := func(name string) float64 {
		f, ok := fams[name]
		if !ok {
			t.Fatalf("%s is absent", name)
		}
		return scalar(t, f)
	}

	ingested := get("traceforge_pipeline_ingested_total")
	stored := get("traceforge_pipeline_stored_total")
	invalid := get("traceforge_pipeline_invalid_total")
	failed := get("traceforge_pipeline_failed_total")

	if ingested != 3 {
		t.Errorf("ingested = %v, want 3", ingested)
	}
	if invalid != 1 {
		t.Errorf("invalid = %v, want 1 (the metric with no name)", invalid)
	}
	if stored != 2 {
		t.Errorf("stored = %v, want 2", stored)
	}
	if sum := stored + invalid + failed; ingested != sum {
		t.Errorf("ingested=%v but stored+invalid+failed=%v; %v metrics are unaccounted for",
			ingested, sum, ingested-sum)
	}
	if _, ok := fams["traceforge_pipeline_dropped_total"]; !ok {
		t.Error("traceforge_pipeline_dropped_total is absent; backpressure would be invisible")
	}
}

// TestStorageGathererCountsWhatIsStored.
func TestStorageGathererCountsWhatIsStored(t *testing.T) {
	store := storage.NewMemoryStorage()
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if err := store.Write(model.Metric{
			Name: "cpu", Type: model.MetricTypeGauge, Value: float64(i),
			Timestamp: now.Add(time.Duration(i) * time.Second),
			Labels:    map[string]string{"host": "a"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Write(model.Metric{
		Name: "cpu", Type: model.MetricTypeGauge, Value: 9, Timestamp: now,
		Labels: map[string]string{"host": "b"},
	}); err != nil {
		t.Fatal(err)
	}

	fams := families(t, StorageGatherer(store))
	if got := scalar(t, fams["traceforge_storage_series"]); got != 2 {
		t.Errorf("series = %v, want 2 (host=a and host=b)", got)
	}
	if got := scalar(t, fams["traceforge_storage_points"]); got != 4 {
		t.Errorf("points = %v, want 4", got)
	}
}

// TestRateLimitGathererTracksTheBucketMap. This gauge is a memory signal as much
// as a traffic one: a client minting a fresh agent id per request grows the map,
// and this is how that is noticed.
func TestRateLimitGathererTracksTheBucketMap(t *testing.T) {
	limiter := ratelimit.New(100, 200)
	g := RateLimitGatherer(limiter)

	if got := scalar(t, families(t, g)["traceforge_ratelimit_buckets"]); got != 0 {
		t.Errorf("buckets = %v on a fresh limiter, want 0", got)
	}

	for _, key := range []string{"agent-1", "agent-2", "agent-3"} {
		limiter.Allow(key)
	}
	if got := scalar(t, families(t, g)["traceforge_ratelimit_buckets"]); got != 3 {
		t.Errorf("buckets = %v after three distinct agents, want 3", got)
	}
}

// TestGatherersDoNotCollide. The handler concatenates every gatherer, and one
// metric name may carry only one HELP line in a document. A collision would make
// a scraper reject the whole scrape.
func TestGatherersDoNotCollide(t *testing.T) {
	store := storage.NewMemoryStorage()
	pipe := pipeline.New(store, pipeline.Config{IngestBuffer: 1, ValidateWorkers: 1, EnrichWorkers: 1, StoreWorkers: 1}, testLogger())
	limiter := ratelimit.New(1, 1)

	var all []promexport.Family
	for _, g := range []promexport.Gatherer{
		BuildInfoGatherer(),
		PipelineGatherer(pipe),
		StorageGatherer(store),
		RateLimitGatherer(limiter),
		NewHTTPMetrics(),
	} {
		all = append(all, g.Gather()...)
	}

	var buf strings.Builder
	if err := promexport.Write(&buf, all); err != nil {
		t.Fatalf("the server's own gatherers do not render cleanly: %v", err)
	}

	seen := make(map[string]bool)
	for _, f := range all {
		if seen[f.Name] {
			t.Errorf("metric %s is gathered by two gatherers", f.Name)
		}
		seen[f.Name] = true
		if !strings.HasPrefix(f.Name, "traceforge_") {
			t.Errorf("metric %s is not namespaced", f.Name)
		}
	}
}
