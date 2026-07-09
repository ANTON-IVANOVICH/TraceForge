package clock

import (
	"fmt"
	"testing"
	"time"
)

// Package-level sinks keep the compiler from discarding the results these loops
// exist to measure.
var (
	sinkTime time.Time
)

// BenchmarkFakeClockAdvance measures Advance as a function of the number of
// pending waiters. Advance fires due waiters one at a time, and each fire
// rescans the whole waiter slice for the next-earliest (earliestDueLocked), so
// the cost is quadratic in the waiter count — the point of varying it here is to
// make that scaling visible rather than to hide it behind a single N.
func BenchmarkFakeClockAdvance(b *testing.B) {
	for _, waiters := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("waiters=%d", waiters), func(b *testing.B) {
			f := NewFake(time.Unix(0, 0).UTC())
			// Tickers rearm after each fire, so every Advance below fires all of
			// them again: steady, comparable work per iteration.
			for i := 0; i < waiters; i++ {
				f.NewTicker(time.Second)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				f.Advance(time.Second)
			}
		})
	}
}

// BenchmarkRealClockNow bounds the overhead of the production Clock's hottest
// method; it should be indistinguishable from a bare time.Now.
func BenchmarkRealClockNow(b *testing.B) {
	c := New()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkTime = c.Now()
	}
}
