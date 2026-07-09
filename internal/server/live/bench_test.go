package live

import (
	"context"
	"fmt"
	"io"
	"testing"

	"metrics-system/internal/model"
	"metrics-system/internal/testutil"
)

// benchMetrics builds a publish payload across 16 series names — a realistic
// working set, not one repeated metric. No tenant label, so every client with an
// empty (unrestricted) scope keeps the whole slice and pays the full marshal.
func benchMetrics(n int) []model.Metric {
	out := make([]model.Metric, n)
	for i := range out {
		out[i] = model.Metric{
			Name:      fmt.Sprintf("cpu_%d", i%16),
			Type:      model.MetricTypeGauge,
			Value:     float64(i),
			Timestamp: testutil.BaseTime,
			Labels:    map[string]string{"host": fmt.Sprintf("web-%d", i%8)},
		}
	}
	return out
}

// BenchmarkHubPublishMetrics measures the caller-facing cost of PublishMetrics —
// the copy plus the non-blocking hand-off to the hub goroutine — as connected
// clients scale 0..100. It is expected to stay roughly flat: the per-client
// fanout happens asynchronously in Run, not on the caller's thread. That
// asymmetry is the design (the pipeline must never be slowed by a dashboard), and
// BenchmarkHubBroadcastFanout measures the cost that was moved off this path.
func BenchmarkHubPublishMetrics(b *testing.B) {
	ms := benchMetrics(32)
	for _, n := range []int{0, 1, 10, 100} {
		b.Run(fmt.Sprintf("clients=%d", n), func(b *testing.B) {
			hub := discardHub()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go hub.Run(ctx)

			conns := make([]io.Closer, 0, n)
			for i := 0; i < n; i++ {
				cl, sc := wsPair()
				hub.Add(sc, "", false)
				go func() { _, _ = io.Copy(io.Discard, cl.conn) }() // drain so writePump never blocks
				conns = append(conns, cl.conn)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				hub.PublishMetrics(ms)
			}
			b.StopTimer()
			for _, c := range conns {
				_ = c.Close()
			}
		})
	}
}

// BenchmarkHubBroadcastFanout isolates the fanout the hub goroutine performs per
// event. broadcastMetrics marshals once per client (the payload is tenant-scoped,
// so it cannot be shared), which makes the cost linear in client count — this is
// where a large dashboard population is actually paid for, and the sub-benchmarks
// make that slope visible.
//
// White-box on purpose: it drives broadcastMetrics directly on a populated client
// set, so no channel scheduling or socket I/O muddies the marshal+fanout measure.
// The send buffers are never drained; deliver's non-blocking send simply fails
// once full, exactly as in production, and the marshal cost is incurred all the
// same.
func BenchmarkHubBroadcastFanout(b *testing.B) {
	ms := benchMetrics(32)
	for _, clients := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("clients=%d", clients), func(b *testing.B) {
			hub := discardHub()
			for i := 0; i < clients; i++ {
				hub.clients[&client{send: make(chan []byte, 64)}] = struct{}{}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				hub.broadcastMetrics(ms)
			}
		})
	}
}
