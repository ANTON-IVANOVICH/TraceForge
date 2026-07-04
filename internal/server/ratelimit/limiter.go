// Package ratelimit provides a per-key token-bucket limiter, used to throttle
// each agent independently.
package ratelimit

import (
	"sync"

	"golang.org/x/time/rate"
)

// PerAgentLimiter keeps one token bucket per key (agent id or client IP).
type PerAgentLimiter struct {
	mu       sync.RWMutex
	limiters map[string]*rate.Limiter
	rps      rate.Limit
	burst    int
}

// New returns a limiter that allows rps requests/second with the given burst
// for each distinct key.
func New(rps float64, burst int) *PerAgentLimiter {
	if burst < 1 {
		burst = 1
	}
	return &PerAgentLimiter{
		limiters: make(map[string]*rate.Limiter),
		rps:      rate.Limit(rps),
		burst:    burst,
	}
}

// Allow reports whether a request for key may proceed, creating the key's
// bucket on first use. Uses double-checked locking: the common case takes only
// a read lock.
func (l *PerAgentLimiter) Allow(key string) bool {
	l.mu.RLock()
	lim, ok := l.limiters[key]
	l.mu.RUnlock()

	if !ok {
		l.mu.Lock()
		// Re-check: another goroutine may have created it while we waited.
		if lim, ok = l.limiters[key]; !ok {
			lim = rate.NewLimiter(l.rps, l.burst)
			l.limiters[key] = lim
		}
		l.mu.Unlock()
	}
	return lim.Allow()
}
