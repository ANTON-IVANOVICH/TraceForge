package silence

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"metrics-system/internal/alerting/alert"
	"metrics-system/internal/clock"
)

// Silence mutes any alert whose labels match all of its Matchers, for the
// half-open window [StartsAt, EndsAt). A non-empty TenantID scopes the silence to
// a single tenant; an empty TenantID (auth disabled) can mute anything.
type Silence struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	Matchers  []Matcher `json:"matchers"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	CreatedBy string    `json:"created_by"`
	Comment   string    `json:"comment"`
	CreatedAt time.Time `json:"created_at"`
}

// Validate checks the invariants that make a silence safe to store: an identity,
// at least one matcher (a matcher-less silence would mute everything), a
// non-empty window, and matchers that are actually compiled.
func (s *Silence) Validate() error {
	if s == nil {
		return errors.New("silence is nil")
	}
	if strings.TrimSpace(s.ID) == "" {
		return errors.New("silence id is required")
	}
	if len(s.Matchers) == 0 {
		return errors.New("silence needs at least one matcher")
	}
	for _, m := range s.Matchers {
		if err := m.valid(); err != nil {
			return err
		}
	}
	if !s.EndsAt.After(s.StartsAt) {
		return errors.New("silence ends_at must be after starts_at")
	}
	return nil
}

// Active reports whether now falls in the silence window [StartsAt, EndsAt).
func (s *Silence) Active(now time.Time) bool {
	return !now.Before(s.StartsAt) && now.Before(s.EndsAt)
}

// clone deep-copies the silence so the store and its callers never share mutable
// state. The compiled regexes inside each Matcher are immutable and safe to share.
func (s *Silence) clone() *Silence {
	if s == nil {
		return nil
	}
	cp := *s
	if s.Matchers != nil {
		cp.Matchers = append([]Matcher(nil), s.Matchers...)
	}
	return &cp
}

// Silencer is the concurrent store of silences and the mute decision they drive.
// It is safe for many readers (Mutes, List, Get) alongside occasional writers
// (Set, Delete, GC).
type Silencer struct {
	clk clock.Clock

	mu   sync.RWMutex
	byID map[string]*Silence
}

// NewSilencer returns an empty silencer. A nil clock defaults to the real clock,
// so callers that do not need determinism can pass nil.
func NewSilencer(clk clock.Clock) *Silencer {
	if clk == nil {
		clk = clock.New()
	}
	return &Silencer{clk: clk, byID: make(map[string]*Silence)}
}

// Set validates and stores a clone of sil, creating it or replacing an existing
// silence with the same ID. Cloning severs the caller's reference so a later
// mutation of their Silence cannot silently change what is muted.
func (s *Silencer) Set(sil *Silence) error {
	if err := sil.Validate(); err != nil {
		return fmt.Errorf("invalid silence: %w", err)
	}
	cp := sil.clone()
	s.mu.Lock()
	s.byID[cp.ID] = cp
	s.mu.Unlock()
	return nil
}

// Get returns a clone of the silence with the given ID.
func (s *Silencer) Get(id string) (*Silence, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sil, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	return sil.clone(), true
}

// Delete removes a silence, reporting whether it existed.
func (s *Silencer) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return false
	}
	delete(s.byID, id)
	return true
}

// List returns clones of the stored silences, sorted by ID. tenant "" returns
// all of them; a non-empty tenant returns only that tenant's silences.
func (s *Silencer) List(tenant string) []*Silence {
	s.mu.RLock()
	out := make([]*Silence, 0, len(s.byID))
	for _, sil := range s.byID {
		if tenant == "" || sil.TenantID == tenant {
			out = append(out, sil.clone())
		}
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Mutes reports whether any active silence covers the alert. It is the hot path
// (called for every alert on every evaluation), so it only clones nothing and
// holds a read lock.
//
// Tenant isolation is enforced here, not just at Set: a tenant-scoped silence may
// mute only alerts carrying the same tenant label, so tenant-a can never silence
// tenant-b's alerts even if their labels otherwise match.
func (s *Silencer) Mutes(a *alert.Alert) bool {
	if a == nil {
		return false
	}
	now := s.clk.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sil := range s.byID {
		if !sil.Active(now) {
			continue
		}
		if sil.TenantID != "" && sil.TenantID != a.Tenant() {
			continue
		}
		if MatchAll(sil.Matchers, a.Labels) {
			return true
		}
	}
	return false
}

// GC drops silences whose window has already closed (EndsAt at or before now), so
// an expired silence cannot accumulate forever or briefly resurrect a mute if a
// clock were to skew backward.
func (s *Silencer) GC() {
	now := s.clk.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sil := range s.byID {
		if !sil.EndsAt.After(now) {
			delete(s.byID, id)
		}
	}
}
