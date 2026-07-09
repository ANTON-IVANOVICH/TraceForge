package ratelimit

import (
	"strconv"
	"sync/atomic"
	"testing"
)

var sinkBool bool

// BenchmarkLimiterAllow contrasts the two production shapes of the workload.
//
// "hot_key" is one agent hammering a single bucket: every call takes the read
// lock and then the bucket's own mutex, so under RunParallel it exposes the
// contention on that shared lock — the reason Allow is on the request hot path's
// critical section.
//
// "distinct_keys" spreads calls across many buckets: the read lock is
// uncontended but the map is large, so this is the lookup/allocation profile
// seen when thousands of agents report at once.
func BenchmarkLimiterAllow(b *testing.B) {
	b.Run("hot_key/serial", func(b *testing.B) {
		l := New(1e9, 1e9) // effectively unlimited: measure the machinery, not refusals
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkBool = l.Allow("agent-hot")
		}
	})

	b.Run("hot_key/parallel", func(b *testing.B) {
		l := New(1e9, 1e9)
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			var local bool
			for pb.Next() {
				local = l.Allow("agent-hot")
			}
			sinkBool = local
		})
	})

	const keys = 1000
	ids := make([]string, keys)
	for i := range ids {
		ids[i] = "agent-" + strconv.Itoa(i)
	}

	b.Run("distinct_keys/serial", func(b *testing.B) {
		l := New(1e9, 1e9)
		for _, id := range ids { // pre-create buckets: measure steady-state lookup, not first-touch
			l.Allow(id)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkBool = l.Allow(ids[i%keys])
		}
	})

	b.Run("distinct_keys/parallel", func(b *testing.B) {
		l := New(1e9, 1e9)
		for _, id := range ids {
			l.Allow(id)
		}
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			var i int
			var local bool
			for pb.Next() {
				local = l.Allow(ids[i%keys])
				i++
			}
			sinkBool = local
		})
	})
}

// BenchmarkLimiterAllowFirstTouch isolates the slow path: a never-seen key, which
// takes the write lock, allocates a bucket and inserts into the map. Every key
// is unique, so the map grows to b.N entries — exactly the profile an
// attacker-controlled key space produces (see TestLimiterMapGrowsUnbounded).
func BenchmarkLimiterAllowFirstTouch(b *testing.B) {
	l := New(1e9, 1e9)
	var ctr atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var local bool
		for pb.Next() {
			local = l.Allow(strconv.FormatUint(ctr.Add(1), 10))
		}
		sinkBool = local
	})
}
