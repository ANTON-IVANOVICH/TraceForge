package rules

import (
	"errors"
	"sort"
	"sync"
)

// ErrRuleNotFound is returned by RuleStore.Get and Delete for an unknown ID.
var ErrRuleNotFound = errors.New("rule not found")

// RuleStore persists rule definitions.
type RuleStore interface {
	// List returns the rules of one tenant, or every rule when tenant is empty.
	List(tenant string) ([]*Rule, error)
	Get(id string) (*Rule, error)
	Put(r *Rule) error
	Delete(id string) error
}

// StateStore persists the evaluator's per-fingerprint alert state. It is what
// makes `for` semantics survive across evaluations.
type StateStore interface {
	GetByRule(ruleID string) ([]*AlertState, error)
	Put(s *AlertState) error
	Delete(ruleID, fingerprint string) error
	DeleteRule(ruleID string) error
	All() ([]*AlertState, error)
}

// MemoryRuleStore keeps rules in memory. Every accessor returns clones, so a
// caller mutating a rule cannot corrupt a runner that is mid-evaluation.
type MemoryRuleStore struct {
	mu    sync.RWMutex
	rules map[string]*Rule
}

// NewMemoryRuleStore returns an empty rule store.
func NewMemoryRuleStore() *MemoryRuleStore {
	return &MemoryRuleStore{rules: make(map[string]*Rule)}
}

// List returns the tenant's rules sorted by ID.
func (s *MemoryRuleStore) List(tenant string) ([]*Rule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*Rule, 0, len(s.rules))
	for _, r := range s.rules {
		if tenant != "" && r.TenantID != tenant {
			continue
		}
		out = append(out, r.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get returns one rule, or ErrRuleNotFound.
func (s *MemoryRuleStore) Get(id string) (*Rule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	r, ok := s.rules[id]
	if !ok {
		return nil, ErrRuleNotFound
	}
	return r.Clone(), nil
}

// Put stores a compiled rule. An uncompiled rule is rejected: scheduling one
// would panic on the first evaluation.
func (s *MemoryRuleStore) Put(r *Rule) error {
	if r == nil || r.ID == "" {
		return errors.New("rule id is required")
	}
	if r.Compiled() == nil {
		return errors.New("rule must be compiled before it is stored")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules[r.ID] = r.Clone()
	return nil
}

// Delete removes a rule, or reports ErrRuleNotFound.
func (s *MemoryRuleStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rules[id]; !ok {
		return ErrRuleNotFound
	}
	delete(s.rules, id)
	return nil
}

// MemoryStateStore keeps alert state in memory, indexed by rule then
// fingerprint. Like the rule store it hands out clones only.
type MemoryStateStore struct {
	mu     sync.RWMutex
	byRule map[string]map[string]*AlertState
}

// NewMemoryStateStore returns an empty state store.
func NewMemoryStateStore() StateStore {
	return &MemoryStateStore{byRule: make(map[string]map[string]*AlertState)}
}

// GetByRule returns every state of one rule, sorted by fingerprint.
func (s *MemoryStateStore) GetByRule(ruleID string) ([]*AlertState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	states := s.byRule[ruleID]
	out := make([]*AlertState, 0, len(states))
	for _, st := range states {
		out = append(out, st.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fingerprint < out[j].Fingerprint })
	return out, nil
}

// Put inserts or replaces one state.
func (s *MemoryStateStore) Put(st *AlertState) error {
	if st == nil || st.RuleID == "" || st.Fingerprint == "" {
		return errors.New("alert state needs a rule id and a fingerprint")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	byFP := s.byRule[st.RuleID]
	if byFP == nil {
		byFP = make(map[string]*AlertState)
		s.byRule[st.RuleID] = byFP
	}
	byFP[st.Fingerprint] = st.Clone()
	return nil
}

// Delete drops one state.
func (s *MemoryStateStore) Delete(ruleID, fingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if byFP := s.byRule[ruleID]; byFP != nil {
		delete(byFP, fingerprint)
		if len(byFP) == 0 {
			delete(s.byRule, ruleID)
		}
	}
	return nil
}

// DeleteRule forgets everything about a rule (called when it is deleted).
func (s *MemoryStateStore) DeleteRule(ruleID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byRule, ruleID)
	return nil
}

// All returns every state, sorted by rule then fingerprint.
func (s *MemoryStateStore) All() ([]*AlertState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []*AlertState
	for _, byFP := range s.byRule {
		for _, st := range byFP {
			out = append(out, st.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RuleID != out[j].RuleID {
			return out[i].RuleID < out[j].RuleID
		}
		return out[i].Fingerprint < out[j].Fingerprint
	})
	return out, nil
}
