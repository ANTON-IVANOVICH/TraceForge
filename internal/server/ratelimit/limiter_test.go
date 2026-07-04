package ratelimit

import "testing"

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
