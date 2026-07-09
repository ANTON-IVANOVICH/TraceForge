package rules

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"metrics-system/internal/alerting/alert"
	"metrics-system/internal/clock"
)

// resolveTimeout bounds how long removing a rule waits to hand its resolutions
// to the notifier.
const resolveTimeout = 2 * time.Second

// QuerierFactory builds the querier a rule evaluates against. The manager calls
// it per rule with that rule's tenant, which is how tenant scoping reaches the
// storage layer.
type QuerierFactory func(tenant string) Querier

// Manager schedules rule evaluation: one goroutine per enabled rule, ticking on
// that rule's own interval. Rules can be added, replaced and removed while it
// runs, without restarting the server.
type Manager struct {
	store     RuleStore
	querierFn QuerierFactory
	states    StateStore
	evaluator *Evaluator
	clk       clock.Clock
	logger    *slog.Logger

	mu      sync.Mutex
	runners map[string]*ruleRunner
	ctx     context.Context // set by Start; the parent of every runner context
	stopped bool
	wg      sync.WaitGroup
}

type ruleRunner struct {
	rule   *Rule
	cancel context.CancelFunc
	done   chan struct{}
}

// NewManager wires the scheduler. out receives the alerts produced by evaluation.
func NewManager(
	store RuleStore,
	querierFn QuerierFactory,
	states StateStore,
	out chan<- *alert.Alert,
	clk clock.Clock,
	logger *slog.Logger,
) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	if clk == nil {
		clk = clock.New()
	}
	return &Manager{
		store:     store,
		querierFn: querierFn,
		states:    states,
		evaluator: NewEvaluator(states, out, logger),
		clk:       clk,
		logger:    logger,
		runners:   make(map[string]*ruleRunner),
	}
}

// Start loads every rule and schedules the enabled ones. It returns immediately;
// evaluation happens in the background until ctx is cancelled or Stop is called.
func (m *Manager) Start(ctx context.Context) error {
	// Publish the context before listing. A rule created concurrently with Start
	// then either schedules itself through Apply or appears in the listing below;
	// listing first would let one slip through both.
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	m.ctx = ctx
	m.mu.Unlock()

	rs, err := m.store.List("")
	if err != nil {
		return err
	}
	for _, r := range rs {
		m.Apply(r) // idempotent: an already-running rule is replaced
	}
	return nil
}

// Apply (re)schedules a rule. An existing runner for the same ID is stopped and
// awaited first, so a rule never has two runners racing to evaluate it. A
// disabled rule simply stops its runner.
//
// The whole operation holds the lock. Releasing it to await the old runner would
// let two concurrent Apply calls for the same ID both observe "no runner" and
// start one each; the loser would be dropped from the map, never cancelled, and
// Stop would then wait for it forever. Holding the lock is safe because a runner
// never acquires it.
func (m *Manager) Apply(r *Rule) {
	// A rule that stops evaluating owes a resolution for whatever it left firing,
	// which is exactly what Remove does.
	if !r.Enabled {
		m.Remove(r)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.stopped || m.ctx == nil {
		return
	}
	m.stopRunnerLocked(r.ID)

	runnerCtx, cancel := context.WithCancel(m.ctx)
	runner := &ruleRunner{rule: r, cancel: cancel, done: make(chan struct{})}
	m.runners[r.ID] = runner

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.run(runnerCtx, runner)
	}()
}

// stopRunnerLocked cancels a rule's runner and waits for it to exit.
func (m *Manager) stopRunnerLocked(id string) {
	old, ok := m.runners[id]
	if !ok {
		return
	}
	delete(m.runners, id)
	old.cancel()
	<-old.done
}

// Remove stops a rule's runner, resolves whatever it left firing, and forgets
// its alert state.
func (m *Manager) Remove(r *Rule) {
	m.mu.Lock()
	m.stopRunnerLocked(r.ID)
	stopped := m.stopped
	m.mu.Unlock()

	if stopped {
		return // shutting down: nobody is listening for the resolutions
	}
	// Emitted outside the lock, and on its own deadline, so an unresponsive
	// notifier cannot block the API request that triggered the removal — nor
	// deadlock against Stop, which needs the same lock.
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()

	if err := m.evaluator.ResolveAll(ctx, r, m.clk.Now()); err != nil {
		m.logger.Warn("resolving a removed rule's alerts failed",
			"rule_id", r.ID, "rule", r.Name, "error", err)
	}
}

// Stop cancels every runner and waits for them. It is idempotent and safe to
// call concurrently with Apply.
func (m *Manager) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		m.wg.Wait()
		return
	}
	m.stopped = true
	runners := make([]*ruleRunner, 0, len(m.runners))
	for _, r := range m.runners {
		runners = append(runners, r)
	}
	m.runners = make(map[string]*ruleRunner)
	m.mu.Unlock()

	for _, r := range runners {
		r.cancel()
	}
	m.wg.Wait()
}

// ActiveAlerts reports the pending and firing alerts, filtered to one tenant.
func (m *Manager) ActiveAlerts(tenant string) []*AlertState {
	all, err := m.states.All()
	if err != nil {
		m.logger.Warn("list alert state failed", "error", err)
		return nil
	}
	out := make([]*AlertState, 0, len(all))
	for _, st := range all {
		if st.State != StateFiring && st.State != StatePending {
			continue
		}
		if tenant != "" && st.Labels["tenant"] != tenant {
			continue
		}
		out = append(out, st)
	}
	return out
}

// run evaluates one rule until its context is cancelled.
func (m *Manager) run(ctx context.Context, r *ruleRunner) {
	defer close(r.done)

	interval := r.rule.Interval.D()
	if interval <= 0 {
		interval = DefaultInterval
	}

	// Jitter the first tick. Without it every rule loaded at startup evaluates on
	// the same instant forever after, turning a steady read load into a periodic
	// burst against storage.
	//
	// A timer rather than After: a cancelled runner must release it immediately
	// instead of leaving it armed until the delay elapses.
	jitter := m.clk.NewTimer(randomDelay(interval))
	select {
	case <-ctx.Done():
		jitter.Stop()
		return
	case <-jitter.C():
	}

	ticker := m.clk.NewTicker(interval)
	defer ticker.Stop()

	for {
		m.evaluateOnce(ctx, r.rule, interval)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
		}
	}
}

// evaluateOnce bounds one evaluation by the rule's interval, so a query that
// hangs cannot stall every subsequent tick of that rule.
func (m *Manager) evaluateOnce(ctx context.Context, rule *Rule, interval time.Duration) {
	evalCtx, cancel := context.WithTimeout(ctx, interval)
	defer cancel()

	start := m.clk.Now()
	err := m.evaluator.Evaluate(evalCtx, rule, m.querierFn(rule.TenantID), start)
	if err != nil && ctx.Err() == nil {
		m.logger.Error("rule evaluation failed",
			"rule_id", rule.ID, "rule", rule.Name, "error", err)
	}
}

// randomDelay returns a uniform delay in [0, interval).
func randomDelay(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(interval)))
}
