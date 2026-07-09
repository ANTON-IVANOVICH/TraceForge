package notify

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"metrics-system/internal/clock"
)

// CircuitState is the breaker's position in its state machine.
//
//	         failure threshold                 cooldown elapsed
//	Closed ────────────────────▶ Open ────────────────────▶ HalfOpen
//	  ▲                          ▲                              │
//	  └──── success threshold ───┴──────── probe fails ─────────┘
type CircuitState int32

const (
	StateClosed CircuitState = iota
	StateOpen
	StateHalfOpen
)

func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// ErrCircuitOpen is returned instead of calling a receiver whose circuit is open.
var ErrCircuitOpen = errors.New("circuit breaker open")

// CircuitBreaker stops calling a receiver that keeps failing. Without one, a
// dead SMTP server means every alert spawns a goroutine that blocks for the full
// dial timeout: a thousand alerts, a thousand stuck goroutines, and a backlog
// that outlives the outage.
//
// The hot path (allow, recordResult) is lock-free; the mutex is taken only for
// the rare state transitions, and always with a re-check inside.
type CircuitBreaker struct {
	failureThreshold int32
	successThreshold int32
	timeout          time.Duration
	clk              clock.Clock

	state     atomic.Int32 // CircuitState
	failures  atomic.Int32
	successes atomic.Int32
	openedAt  atomic.Int64 // unix nanos

	// probing admits exactly one trial call while half-open. A burst of alerts
	// must not all hit a backend that is only just recovering.
	probing atomic.Bool

	mu sync.Mutex
}

// NewCircuitBreaker returns a closed breaker. failureThreshold consecutive
// failures open it; after timeout it admits one probe; successThreshold
// consecutive successful probes close it again.
func NewCircuitBreaker(failureThreshold, successThreshold int, timeout time.Duration, clk clock.Clock) *CircuitBreaker {
	if failureThreshold <= 0 {
		failureThreshold = 5
	}
	if successThreshold <= 0 {
		successThreshold = 1
	}
	if timeout <= 0 {
		timeout = time.Minute
	}
	if clk == nil {
		clk = clock.New()
	}
	return &CircuitBreaker{
		failureThreshold: int32(failureThreshold),
		successThreshold: int32(successThreshold),
		timeout:          timeout,
		clk:              clk,
	}
}

// State reports the current state (for tests and diagnostics).
func (cb *CircuitBreaker) State() CircuitState { return CircuitState(cb.state.Load()) }

// Call runs fn unless the circuit is open, and records the outcome.
func (cb *CircuitBreaker) Call(fn func() error) error {
	probe, ok := cb.allow()
	if !ok {
		return ErrCircuitOpen
	}
	err := fn()
	cb.recordResult(err, probe)
	return err
}

// allow reports whether a call may proceed, and whether it is the half-open
// probe (whose completion must release the probe slot).
func (cb *CircuitBreaker) allow() (probe, ok bool) {
	switch CircuitState(cb.state.Load()) {
	case StateClosed:
		return false, true

	case StateOpen:
		if cb.clk.Now().Sub(time.Unix(0, cb.openedAt.Load())) < cb.timeout {
			return false, false
		}
		cb.mu.Lock()
		// Re-check: another goroutine may have transitioned already.
		if CircuitState(cb.state.Load()) == StateOpen &&
			cb.clk.Now().Sub(time.Unix(0, cb.openedAt.Load())) >= cb.timeout {
			cb.state.Store(int32(StateHalfOpen))
			cb.successes.Store(0)
			cb.failures.Store(0)
			cb.probing.Store(false)
		}
		cb.mu.Unlock()
		return cb.takeProbe()

	case StateHalfOpen:
		return cb.takeProbe()
	}
	return false, false
}

// takeProbe admits a single in-flight trial call while half-open.
func (cb *CircuitBreaker) takeProbe() (probe, ok bool) {
	if CircuitState(cb.state.Load()) != StateHalfOpen {
		// The state moved on between the check and here; retry the decision.
		return false, CircuitState(cb.state.Load()) == StateClosed
	}
	if !cb.probing.CompareAndSwap(false, true) {
		return false, false
	}
	return true, true
}

func (cb *CircuitBreaker) recordResult(err error, probe bool) {
	if probe {
		defer cb.probing.Store(false)
	}

	if err == nil {
		switch CircuitState(cb.state.Load()) {
		case StateHalfOpen:
			if cb.successes.Add(1) >= cb.successThreshold {
				cb.mu.Lock()
				if CircuitState(cb.state.Load()) == StateHalfOpen {
					cb.state.Store(int32(StateClosed))
					cb.failures.Store(0)
					cb.successes.Store(0)
				}
				cb.mu.Unlock()
			}
		case StateClosed:
			cb.failures.Store(0)
		}
		return
	}

	switch CircuitState(cb.state.Load()) {
	case StateClosed:
		if cb.failures.Add(1) >= cb.failureThreshold {
			cb.trip()
		}
	case StateHalfOpen:
		// The backend is still sick; back off for another full timeout.
		cb.trip()
	}
}

func (cb *CircuitBreaker) trip() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if CircuitState(cb.state.Load()) == StateOpen {
		return
	}
	cb.state.Store(int32(StateOpen))
	cb.openedAt.Store(cb.clk.Now().UnixNano())
	cb.failures.Store(0)
	cb.successes.Store(0)
}
