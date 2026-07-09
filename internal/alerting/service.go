// Package alerting assembles the alerting subsystem: it owns the rule store,
// the evaluation scheduler, the notification pipeline and the silence registry,
// and exposes the tenant-scoped operations the HTTP API needs.
//
// The split it enforces is the one that matters: rule evaluation is
// deterministic, periodic and must not miss a tick; notification delivery is
// slow, unreliable and asynchronous. They meet only over a buffered channel.
package alerting

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"metrics-system/internal/alerting/alert"
	"metrics-system/internal/alerting/config"
	"metrics-system/internal/alerting/inhibit"
	"metrics-system/internal/alerting/notify"
	"metrics-system/internal/alerting/rules"
	"metrics-system/internal/alerting/silence"
	"metrics-system/internal/clock"
	"metrics-system/internal/server/storage"
)

// Errors returned to the API layer, which maps them to status codes.
var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
)

// maxPreviewSteps bounds a backtest so a crafted from/to/step triple cannot turn
// one HTTP request into an unbounded evaluation loop.
const maxPreviewSteps = 500

// Config configures the alerting service.
type Config struct {
	RulesFile   string        // optional bootstrap rules
	ConfigFile  string        // optional receivers/inhibition config
	Lookback    time.Duration // how far back an instant selector may reach
	AlertBuffer int           // evaluator -> notifier channel capacity
	Notify      notify.Config
}

// Service is the alerting subsystem.
type Service struct {
	ruleStore rules.RuleStore
	states    rules.StateStore
	manager   *rules.Manager
	notifier  *notify.Notifier
	silencer  *silence.Silencer

	alerts   chan *alert.Alert
	store    storage.Storage
	lookback time.Duration
	logger   *slog.Logger
}

// New builds the service. It loads and compiles the bootstrap rules and the
// receiver configuration up front: a bad expression or an unreachable receiver
// definition must fail startup, not the first evaluation at 3am.
func New(cfg Config, store storage.Storage, clk clock.Clock, logger *slog.Logger) (*Service, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if clk == nil {
		clk = clock.New()
	}
	if cfg.Lookback <= 0 {
		cfg.Lookback = 5 * time.Minute
	}
	if cfg.AlertBuffer <= 0 {
		cfg.AlertBuffer = 1024
	}

	file := config.Default()
	if cfg.ConfigFile != "" {
		loaded, err := config.Load(cfg.ConfigFile)
		if err != nil {
			return nil, fmt.Errorf("alert config: %w", err)
		}
		file = loaded
	}

	recvs, err := config.BuildReceivers(file, logger)
	if err != nil {
		return nil, fmt.Errorf("alert receivers: %w", err)
	}

	notifyCfg := cfg.Notify
	notifyCfg.GroupBy = file.GroupBy
	notifyCfg.GroupWait = file.GroupWait.D()
	notifyCfg.GroupInterval = file.GroupInterval.D()
	notifyCfg.RepeatInterval = file.RepeatInterval.D()
	notifyCfg.DefaultReceivers = file.DefaultReceivers

	silencer := silence.NewSilencer(clk)
	inhibitor := inhibit.New(file.InhibitRules)

	alerts := make(chan *alert.Alert, cfg.AlertBuffer)
	notifier := notify.New(notifyCfg, alerts, recvs, silencer, inhibitor, clk, logger)

	ruleStore := rules.NewMemoryRuleStore()
	if cfg.RulesFile != "" {
		loaded, err := config.LoadRules(cfg.RulesFile)
		if err != nil {
			return nil, fmt.Errorf("alert rules: %w", err)
		}
		for _, r := range loaded {
			if err := ruleStore.Put(r); err != nil {
				return nil, fmt.Errorf("alert rules: %s: %w", r.ID, err)
			}
		}
		logger.Info("alerting: rules loaded", "count", len(loaded), "file", cfg.RulesFile)
	}

	states := rules.NewMemoryStateStore()
	querierFor := func(tenant string) rules.Querier {
		return rules.NewStorageQuerier(store, tenant, cfg.Lookback)
	}
	manager := rules.NewManager(ruleStore, querierFor, states, alerts, clk, logger)

	return &Service{
		ruleStore: ruleStore,
		states:    states,
		manager:   manager,
		notifier:  notifier,
		silencer:  silencer,
		alerts:    alerts,
		store:     store,
		lookback:  cfg.Lookback,
		logger:    logger,
	}, nil
}

// SetObserver taps every alert entering the notifier (used by the live dashboard).
// Call before Run.
func (s *Service) SetObserver(fn func(*alert.Alert)) { s.notifier.SetObserver(fn) }

// Run starts evaluation and notification and blocks until ctx is cancelled and
// every goroutine has stopped. Rule runners are stopped before the notifier is
// awaited, so nothing is still trying to publish onto the alert channel.
func (s *Service) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.notifier.Run(ctx)
	}()

	if err := s.manager.Start(ctx); err != nil {
		s.manager.Stop()
		wg.Wait()
		return fmt.Errorf("start rule manager: %w", err)
	}
	s.logger.Info("alerting started")

	<-ctx.Done()
	s.manager.Stop()
	wg.Wait()
	s.logger.Info("alerting stopped")
	return nil
}

// ---------------------------------------------------------------------------
// Rules API (tenant-scoped: an empty tenant means auth is off and sees all)
// ---------------------------------------------------------------------------

// ListRules returns the tenant's rules.
func (s *Service) ListRules(tenant string) ([]*rules.Rule, error) {
	return s.ruleStore.List(tenant)
}

// GetRule returns one rule, or ErrNotFound when it does not exist or belongs to
// another tenant. Reporting "not found" rather than "forbidden" for a foreign
// rule keeps a tenant from probing which rule IDs exist.
func (s *Service) GetRule(tenant, id string) (*rules.Rule, error) {
	r, err := s.ruleStore.Get(id)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	if !ownsRule(tenant, r) {
		return nil, ErrNotFound
	}
	return r, nil
}

// PutRule compiles, stores and (re)schedules a rule. The tenant is taken from
// the caller, never from the request body, so a tenant cannot plant a rule that
// evaluates against another tenant's data.
func (s *Service) PutRule(tenant string, r *rules.Rule) (*rules.Rule, error) {
	if r.ID == "" {
		r.ID = newID()
	}
	if existing, err := s.ruleStore.Get(r.ID); err == nil && !ownsRule(tenant, existing) {
		return nil, ErrNotFound
	}
	r.TenantID = tenant

	now := time.Now().UTC()
	r.UpdatedAt = now
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	if err := r.Compile(); err != nil {
		return nil, err
	}
	if err := s.ruleStore.Put(r); err != nil {
		return nil, err
	}
	s.manager.Apply(r)
	return r, nil
}

// DeleteRule removes a rule, stops its runner and forgets its alert state.
func (s *Service) DeleteRule(tenant, id string) error {
	r, err := s.ruleStore.Get(id)
	if err != nil {
		return mapStoreErr(err)
	}
	if !ownsRule(tenant, r) {
		return ErrNotFound
	}
	// Remove announces the end of whatever the rule left firing before the rule
	// itself disappears.
	s.manager.Remove(r)
	return s.ruleStore.Delete(id)
}

// PreviewResult is one instant of a rule backtest.
type PreviewResult struct {
	At      time.Time      `json:"at"`
	Samples []rules.Sample `json:"samples"`
}

// Preview evaluates an expression over historical data without storing anything,
// so a user can see what a rule would have fired on before committing to it.
func (s *Service) Preview(ctx context.Context, tenant, expr string, from, to time.Time, step time.Duration) ([]PreviewResult, error) {
	compiled, _, err := rules.Parse(expr)
	if err != nil {
		return nil, err
	}
	if step <= 0 {
		step = time.Minute
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	if from.IsZero() {
		from = to.Add(-time.Hour)
	}
	if !from.Before(to) {
		return nil, errors.New("from must be before to")
	}
	if steps := to.Sub(from) / step; steps > maxPreviewSteps {
		return nil, fmt.Errorf("preview window too large: %d steps (max %d), widen step", steps, maxPreviewSteps)
	}

	q := rules.NewStorageQuerier(s.store, tenant, s.lookback)
	var out []PreviewResult
	for t := from; !t.After(to); t = t.Add(step) {
		vec, err := compiled.Eval(ctx, q, t)
		if err != nil {
			return nil, err
		}
		if len(vec) > 0 {
			out = append(out, PreviewResult{At: t, Samples: vec})
		}
	}
	if out == nil {
		out = []PreviewResult{}
	}
	return out, nil
}

// ActiveAlerts lists the tenant's pending and firing alerts.
func (s *Service) ActiveAlerts(tenant string) []*rules.AlertState {
	return s.manager.ActiveAlerts(tenant)
}

// ---------------------------------------------------------------------------
// Silences API
// ---------------------------------------------------------------------------

// ListSilences returns the tenant's silences.
func (s *Service) ListSilences(tenant string) []*silence.Silence {
	return s.silencer.List(tenant)
}

// PutSilence creates or replaces a silence owned by the caller's tenant.
func (s *Service) PutSilence(tenant string, sil *silence.Silence) (*silence.Silence, error) {
	if sil.ID == "" {
		sil.ID = newID()
	}
	if existing, ok := s.silencer.Get(sil.ID); ok && !ownsSilence(tenant, existing) {
		return nil, ErrNotFound
	}
	sil.TenantID = tenant
	if sil.CreatedAt.IsZero() {
		sil.CreatedAt = time.Now().UTC()
	}
	if sil.StartsAt.IsZero() {
		sil.StartsAt = time.Now().UTC()
	}
	if err := s.silencer.Set(sil); err != nil {
		return nil, err
	}
	return sil, nil
}

// DeleteSilence removes a silence the caller owns.
func (s *Service) DeleteSilence(tenant, id string) error {
	sil, ok := s.silencer.Get(id)
	if !ok || !ownsSilence(tenant, sil) {
		return ErrNotFound
	}
	s.silencer.Delete(id)
	return nil
}

// ---------------------------------------------------------------------------

func ownsRule(tenant string, r *rules.Rule) bool {
	return tenant == "" || r.TenantID == tenant
}

func ownsSilence(tenant string, s *silence.Silence) bool {
	return tenant == "" || s.TenantID == tenant
}

func mapStoreErr(err error) error {
	if errors.Is(err, rules.ErrRuleNotFound) {
		return ErrNotFound
	}
	return err
}

// newID returns a random, URL-safe identifier. crypto/rand keeps IDs
// unguessable, which matters because an ID is the only handle on a silence.
func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand cannot fail on any supported platform; if it somehow does,
		// a time-derived id is still unique enough to avoid collisions.
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))[:16]
	}
	return hex.EncodeToString(b[:])
}
