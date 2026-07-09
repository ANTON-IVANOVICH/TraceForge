// Package ratelimit provides a per-key token-bucket limiter, used to throttle
// each agent independently.
package ratelimit

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// The limiter's key is an agent id, or a client IP when auth is off. Both are
// chosen by the caller, so the number of distinct keys is chosen by the caller
// too. A map with one bucket per key and no eviction therefore hands anyone who
// can reach the ingest endpoint a memory-exhaustion primitive: a few million
// requests with a fresh agent id each, and the process is killed by the OOM
// reaper — while every individual request was inside its rate limit.
//
// Two bounds close it. Idle buckets are swept, so honest churn (agents restart
// with new ids, IPs rotate) costs nothing over time. And a hard cap evicts the
// least recently seen bucket, so even a burst faster than the sweep interval
// cannot grow the map past a fixed size.
//
// Evicting a bucket forgives whatever debt it had accrued. That is acceptable:
// the evicted bucket is by construction the one nobody has used, and re-creating
// it grants at most one burst.
const (
	// idleTTL is how long a bucket outlives its last request. It must exceed the
	// bucket's own refill time or a steady low-rate client would be swept between
	// requests and get a fresh burst each time.
	idleTTL = 10 * time.Minute

	// sweepEvery bounds the amortised cost of sweeping: one O(n) scan per this
	// many newly created buckets.
	sweepEvery = 1024

	// defaultMaxKeys is the hard ceiling. At ~200 bytes per bucket this is a few
	// tens of megabytes — large enough that a real deployment never reaches it,
	// small enough that an attacker gains nothing by trying.
	defaultMaxKeys = 100_000

	// evictFraction is the share of the map dropped when the cap is hit. Evicting
	// one bucket per insert would make every request past the cap pay an O(n)
	// scan for the oldest entry — trading a memory-exhaustion attack for a
	// cheaper CPU-exhaustion one. Evicting a slice of them amortises that scan
	// over thousands of inserts.
	evictFraction = 16
)

type bucket struct {
	limiter *rate.Limiter
	// lastSeen is atomic so the read path can refresh it under the read lock,
	// without promoting to the write lock on every allowed request.
	lastSeen atomic.Int64 // unix nanoseconds
}

// PerAgentLimiter keeps one token bucket per key (agent id or client IP).
type PerAgentLimiter struct {
	mu         sync.RWMutex
	buckets    map[string]*bucket
	sinceSweep int // guarded by mu

	rps   rate.Limit
	burst int

	// maxKeys and now are seams for tests: one keeps the eviction test from
	// inserting a hundred thousand buckets, the other keeps the sweep test from
	// sleeping for ten minutes. Production uses the constants and the wall clock.
	maxKeys int
	now     func() time.Time
}

// New returns a limiter that allows rps requests/second with the given burst
// for each distinct key.
func New(rps float64, burst int) *PerAgentLimiter {
	if burst < 1 {
		burst = 1
	}
	return &PerAgentLimiter{
		buckets: make(map[string]*bucket),
		rps:     rate.Limit(rps),
		burst:   burst,
		maxKeys: defaultMaxKeys,
		now:     time.Now,
	}
}

// Allow reports whether a request for key may proceed, creating the key's
// bucket on first use. Double-checked locking keeps the common case — an
// existing bucket — on the read lock.
func (l *PerAgentLimiter) Allow(key string) bool {
	now := l.now()

	l.mu.RLock()
	b, ok := l.buckets[key]
	l.mu.RUnlock()

	if ok {
		b.lastSeen.Store(now.UnixNano())
		return b.limiter.Allow()
	}

	l.mu.Lock()
	// Re-check: another goroutine may have created it while we waited.
	if b, ok = l.buckets[key]; !ok {
		l.sinceSweep++
		if l.sinceSweep >= sweepEvery || len(l.buckets) >= l.maxKeys {
			l.sweepLocked(now)
		}
		if len(l.buckets) >= l.maxKeys {
			l.evictOldestLocked(max(1, l.maxKeys/evictFraction))
		}
		b = &bucket{limiter: rate.NewLimiter(l.rps, l.burst)}
		l.buckets[key] = b
	}
	b.lastSeen.Store(now.UnixNano())
	l.mu.Unlock()

	return b.limiter.Allow()
}

// Len reports how many buckets are currently held. It is the number to watch if
// the eviction ever starts firing in production.
func (l *PerAgentLimiter) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.buckets)
}

func (l *PerAgentLimiter) sweepLocked(now time.Time) {
	cutoff := now.Add(-idleTTL).UnixNano()
	for key, b := range l.buckets {
		if b.lastSeen.Load() < cutoff {
			delete(l.buckets, key)
		}
	}
	l.sinceSweep = 0
}

// evictOldestLocked drops the count least recently used buckets. It runs only
// when the map is at its cap and a sweep freed nothing — that is, under a flood
// of keys nobody has seen before.
//
// Ties at the cutoff are evicted too, so this may remove slightly more than
// count. Removing a few extra idle buckets is free; scanning again to avoid it
// is not.
func (l *PerAgentLimiter) evictOldestLocked(count int) {
	if count >= len(l.buckets) {
		clear(l.buckets)
		return
	}

	seen := make([]int64, 0, len(l.buckets))
	for _, b := range l.buckets {
		seen = append(seen, b.lastSeen.Load())
	}
	slices.Sort(seen)
	cutoff := seen[count-1]

	for key, b := range l.buckets {
		if b.lastSeen.Load() <= cutoff {
			delete(l.buckets, key)
		}
	}
}
