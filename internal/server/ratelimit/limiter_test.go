package ratelimit

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestPerAgentLimiter_Burst(t *testing.T) {
	// rps = 0 => no refill; only the burst tokens are ever available.
	l := New(0, 3)
	allowed := 0
	for i := 0; i < 10; i++ {
		if l.Allow("agent-1") {
			allowed++
		}
	}
	if allowed != 3 {
		t.Errorf("allowed = %d, want 3 (burst size)", allowed)
	}
}

func TestPerAgentLimiter_PerKeyIndependent(t *testing.T) {
	l := New(0, 1)
	if !l.Allow("a") {
		t.Fatal("a first request should be allowed")
	}
	if l.Allow("a") {
		t.Fatal("a second request should be denied (bucket empty)")
	}
	if !l.Allow("b") {
		t.Fatal("b first request should be allowed (independent bucket)")
	}
}

// A key is an agent id or a client IP — both attacker-chosen. Without eviction,
// a stream of distinct keys grows the bucket map until the process is OOM-killed,
// while every single request stays inside its own rate limit.
func TestLimiterCapsTheNumberOfBuckets(t *testing.T) {
	l := New(1e9, 1e9)
	l.maxKeys = 512

	for i := 0; i < 20*l.maxKeys; i++ {
		l.Allow("attacker-" + strconv.Itoa(i))
	}
	if got := l.Len(); got > l.maxKeys {
		t.Fatalf("bucket map holds %d entries, above the cap of %d", got, l.maxKeys)
	}
}

// Evicting a single oldest bucket per insert would make every request past the
// cap pay an O(n) scan — swapping a memory-exhaustion attack for a CPU one. The
// batch eviction must therefore run rarely: once per evictFraction of the cap.
func TestLimiterEvictionIsAmortized(t *testing.T) {
	l := New(1e9, 1e9)
	l.maxKeys = 1024

	var evictions int
	for i := 0; i < 8*l.maxKeys; i++ {
		before := l.Len()
		l.Allow("attacker-" + strconv.Itoa(i))
		if l.Len() < before {
			evictions++
		}
	}

	// Eight cap-fulls of fresh keys, each eviction freeing a 1/evictFraction
	// slice: a handful of scans, not one per insert.
	if maxExpected := 8*evictFraction + 2; evictions > maxExpected {
		t.Errorf("evicted %d times over %d inserts; the scan is not amortized (want <= %d)",
			evictions, 8*l.maxKeys, maxExpected)
	}
	if evictions == 0 {
		t.Error("the cap was never enforced")
	}
}

func TestLimiterSweepsIdleBuckets(t *testing.T) {
	l := New(10, 10)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	// One bucket that will go idle, then enough fresh keys to trigger a sweep.
	l.Allow("idle-agent")
	now = now.Add(idleTTL + time.Minute)
	for i := 0; i < sweepEvery; i++ {
		l.Allow("busy-" + strconv.Itoa(i))
	}

	l.mu.RLock()
	_, stillThere := l.buckets["idle-agent"]
	l.mu.RUnlock()
	if stillThere {
		t.Error("a bucket idle for longer than idleTTL should have been swept")
	}
}

// A bucket must not be swept while its owner is still using it, however slowly:
// re-creating it hands out a fresh burst, which is a rate limit that does not
// limit.
func TestLimiterKeepsBucketsThatAreStillInUse(t *testing.T) {
	l := New(10, 10)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	l.Allow("steady-agent")
	for i := 0; i < sweepEvery; i++ {
		now = now.Add(time.Second)
		l.Allow("steady-agent") // keeps lastSeen fresh
		l.Allow("noise-" + strconv.Itoa(i))
	}

	l.mu.RLock()
	_, stillThere := l.buckets["steady-agent"]
	l.mu.RUnlock()
	if !stillThere {
		t.Error("an active bucket was swept; its owner would get a fresh burst")
	}
}

func TestLimiterEvictionIsRaceFree(t *testing.T) {
	l := New(1e9, 1e9)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				l.Allow("agent-" + strconv.Itoa(g*2000+i))
			}
		}(g)
	}
	wg.Wait()
}

func TestPerAgentLimiter_ConcurrentKeys(t *testing.T) {
	// Exercise the double-checked locking path under -race with many goroutines
	// hitting distinct and shared keys.
	l := New(1000, 1000)
	done := make(chan struct{})
	for g := 0; g < 8; g++ {
		go func(g int) {
			for i := 0; i < 100; i++ {
				l.Allow("shared")
				l.Allow(string(rune('a' + g)))
			}
			done <- struct{}{}
		}(g)
	}
	for g := 0; g < 8; g++ {
		<-done
	}
}
