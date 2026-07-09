package alert

import (
	"io"
	"log/slog"
	"strconv"
	"testing"

	"metrics-system/internal/clock"
)

// sinkStr keeps the compiler from eliding the benchmarked Fingerprint call.
var sinkStr string

func benchLabels(n int) map[string]string {
	m := make(map[string]string, n)
	for i := 0; i < n; i++ {
		m["label_"+strconv.Itoa(i)] = "value_" + strconv.Itoa(i)
	}
	return m
}

// Fingerprint runs once per sample per evaluation — the hottest path in the
// alerting loop — so its cost as a function of label count matters. The sha256
// dominates; the sub-benchmarks show how the per-label framing scales.
func BenchmarkFingerprint(b *testing.B) {
	for _, n := range []int{1, 5, 10, 20} {
		labels := benchLabels(n)
		b.Run("labels="+strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sinkStr = Fingerprint("rule-abc123", labels)
			}
		})
	}
}

// BenchmarkGrouperAdd exercises Ingest, the per-alert hot path: it clones the
// alert, computes the group key, and either inserts or dedups by fingerprint.
// A fixed working set per size keeps the group table bounded so the measurement
// is the ingest cost, not unbounded map growth. All alerts share one group
// (group_by is alertname+tenant), so this also measures the dedup branch.
func BenchmarkGrouperAdd(b *testing.B) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, n := range []int{10, 100} {
		alerts := make([]*Alert, n)
		for i := range alerts {
			alerts[i] = makeBenchAlert(i)
		}
		b.Run("hosts="+strconv.Itoa(n), func(b *testing.B) {
			out := make(chan *Group, 1) // Ingest never sends; flush does
			g := NewGrouper(testCfg(), out, clock.NewFake(baseTime), logger)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				g.Ingest(alerts[i%n])
			}
		})
	}
}

func makeBenchAlert(i int) *Alert {
	labels := map[string]string{
		"alertname": "HighLatency",
		"tenant":    "acme",
		"host":      "web-" + strconv.Itoa(i),
	}
	a := &Alert{
		RuleID:    "rule-latency",
		RuleName:  "HighLatency",
		Status:    StatusFiring,
		Severity:  "warning",
		Labels:    labels,
		StartsAt:  baseTime,
		Value:     float64(i),
		Receivers: []string{"team-a"},
	}
	a.Fingerprint = Fingerprint(a.RuleID, a.Labels)
	return a
}
