package notify

import (
	"container/heap"
	"context"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"metrics-system/internal/alerting/alert"
	"metrics-system/internal/alerting/notify/receivers"
	"metrics-system/internal/clock"
)

// retryTick is how often the queue looks for work that has come due. It bounds
// the lateness of a retry, not its delay.
const retryTick = 200 * time.Millisecond

// RetryPolicy describes exponential backoff with jitter.
type RetryPolicy struct {
	MaxAttempts     int
	InitialInterval time.Duration
	MaxInterval     time.Duration
	Multiplier      float64
	Jitter          float64 // fraction of the delay, e.g. 0.3 for ±30%
}

// DefaultRetryPolicy is a sensible starting point: five attempts, 1s doubling to
// a 5m ceiling, ±30% jitter.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:     5,
		InitialInterval: time.Second,
		MaxInterval:     5 * time.Minute,
		Multiplier:      2.0,
		Jitter:          0.3,
	}
}

// NextDelay returns the delay before the given attempt (1-based).
//
// The jitter is not decoration. A thousand tenants retrying a shared Slack
// webhook on a clean 1s/2s/4s/8s schedule all wake at the same instants and
// re-create the very overload they are backing off from. Spreading each delay by
// ±Jitter breaks up that thundering herd.
func (p RetryPolicy) NextDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	mult := p.Multiplier
	if mult <= 0 {
		mult = 2.0
	}
	delay := float64(p.InitialInterval) * math.Pow(mult, float64(attempt-1))
	if max := float64(p.MaxInterval); max > 0 && delay > max {
		delay = max
	}
	if p.Jitter > 0 {
		delay *= 1 + (rand.Float64()*2-1)*p.Jitter
	}
	if delay < 0 {
		delay = 0
	}
	return time.Duration(delay)
}

// SendFunc performs one delivery attempt. The notifier supplies one that runs
// through the receiver's circuit breaker, so retries and first attempts feed the
// same failure record.
type SendFunc func(ctx context.Context, r receivers.Receiver, g *alert.Group) error

type retryItem struct {
	receiver    receivers.Receiver
	group       *alert.Group
	attempt     int
	nextAttempt time.Time
	index       int // heap position
}

// RetryQueue re-attempts failed deliveries on an exponential backoff. It is an
// in-memory queue: a restart forgets pending retries, which is an accepted
// trade-off at this stage (a durable queue would live in bbolt alongside the
// metric store).
type RetryQueue struct {
	policy   RetryPolicy
	capacity int
	send     SendFunc
	clk      clock.Clock
	logger   *slog.Logger

	mu sync.Mutex
	h  itemHeap

	wg sync.WaitGroup
}

// NewRetryQueue builds the queue. capacity bounds how many deliveries may be
// awaiting a retry before new ones are refused rather than buffered without end.
func NewRetryQueue(policy RetryPolicy, capacity int, send SendFunc, clk clock.Clock, logger *slog.Logger) *RetryQueue {
	if policy.MaxAttempts <= 0 {
		policy = DefaultRetryPolicy()
	}
	if capacity <= 0 {
		capacity = 256
	}
	if clk == nil {
		clk = clock.New()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &RetryQueue{policy: policy, capacity: capacity, send: send, clk: clk, logger: logger}
}

// Len reports how many deliveries are waiting.
func (q *RetryQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.h.Len()
}

// Enqueue schedules attempt number `attempt` (1-based) of delivering g to r.
// It reports false when the attempt budget is spent or the queue is full — the
// caller must log that, because it means a notification is being dropped.
func (q *RetryQueue) Enqueue(r receivers.Receiver, g *alert.Group, attempt int) bool {
	if attempt > q.policy.MaxAttempts {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.h.Len() >= q.capacity {
		return false
	}
	heap.Push(&q.h, &retryItem{
		receiver:    r,
		group:       g,
		attempt:     attempt,
		nextAttempt: q.clk.Now().Add(q.policy.NextDelay(attempt)),
	})
	return true
}

// Run delivers items as they come due, until ctx is cancelled. It waits for
// in-flight deliveries before returning.
func (q *RetryQueue) Run(ctx context.Context) {
	ticker := q.clk.NewTicker(retryTick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			q.wg.Wait()
			return
		case <-ticker.C():
			for _, item := range q.popDue(q.clk.Now()) {
				q.wg.Add(1)
				// One goroutine per due item: a receiver that takes its full send
				// timeout must not hold up every other receiver's retry.
				go func(it *retryItem) {
					defer q.wg.Done()
					q.deliver(ctx, it)
				}(item)
			}
		}
	}
}

// popDue removes every item whose time has come.
func (q *RetryQueue) popDue(now time.Time) []*retryItem {
	q.mu.Lock()
	defer q.mu.Unlock()

	var due []*retryItem
	for q.h.Len() > 0 && !q.h[0].nextAttempt.After(now) {
		due = append(due, heap.Pop(&q.h).(*retryItem))
	}
	return due
}

func (q *RetryQueue) deliver(ctx context.Context, item *retryItem) {
	err := q.send(ctx, item.receiver, item.group)
	if err == nil {
		q.logger.Debug("retry succeeded",
			"receiver", item.receiver.Name(), "group", item.group.Key, "attempt", item.attempt)
		return
	}
	if receivers.IsPermanent(err) {
		q.logger.Error("permanent delivery failure, dropping notification",
			"receiver", item.receiver.Name(), "group", item.group.Key, "error", err)
		return
	}
	if ctx.Err() != nil {
		return // shutting down; the item dies with the process
	}
	if !q.Enqueue(item.receiver, item.group, item.attempt+1) {
		q.logger.Error("giving up on notification",
			"receiver", item.receiver.Name(), "group", item.group.Key,
			"attempts", item.attempt, "error", err)
	}
}

// itemHeap is a min-heap on nextAttempt.
type itemHeap []*retryItem

func (h itemHeap) Len() int           { return len(h) }
func (h itemHeap) Less(i, j int) bool { return h[i].nextAttempt.Before(h[j].nextAttempt) }
func (h itemHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *itemHeap) Push(x any)        { it := x.(*retryItem); it.index = len(*h); *h = append(*h, it) }
func (h *itemHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return it
}
