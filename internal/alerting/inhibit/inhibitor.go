// Package inhibit suppresses alerts that are redundant in the presence of a more
// fundamental one: if a host is down, its CPU alert adds no information and should
// stay quiet. It mirrors Alertmanager's inhibition rules.
package inhibit

import (
	"errors"

	"metrics-system/internal/alerting/alert"
	"metrics-system/internal/alerting/silence"
)

// Rule inhibits a target alert while a source alert is firing. The target is a
// candidate when it matches TargetMatchers; a firing alert is a source when it
// matches SourceMatchers; the two are linked only if they agree on every label in
// Equal (which is what ties "HostDown on web-1" to "CPUHigh on web-1" rather than
// to every CPU alert everywhere).
type Rule struct {
	SourceMatchers []silence.Matcher `json:"source_matchers"`
	TargetMatchers []silence.Matcher `json:"target_matchers"`
	Equal          []string          `json:"equal"`
}

// Validate requires both matcher sets to be non-empty. An empty set matches
// nothing (see silence.MatchAll), so an empty-source or empty-target rule would
// be inert; rejecting it turns a silent no-op into a visible configuration error.
func (r Rule) Validate() error {
	if len(r.SourceMatchers) == 0 {
		return errors.New("inhibit rule needs at least one source matcher")
	}
	if len(r.TargetMatchers) == 0 {
		return errors.New("inhibit rule needs at least one target matcher")
	}
	return nil
}

// Inhibitor decides, against a fixed set of rules, whether an alert is currently
// suppressed. It holds no mutable state and is safe for concurrent use.
type Inhibitor struct {
	rules []Rule
}

// New returns an inhibitor over the given rules. Rules are used as-is: an
// unvalidated rule with an empty matcher set is harmless because it can never
// match, so validation is the caller's concern, not a correctness requirement here.
func New(rules []Rule) *Inhibitor {
	return &Inhibitor{rules: rules}
}

// Inhibited reports whether target should be suppressed by any firing alert.
//
// Three guards keep inhibition from misfiring:
//   - an alert never inhibits itself (fingerprints compared),
//   - only alerts with status firing may act as a source,
//   - a source may inhibit only a target of the same tenant, so one tenant cannot
//     mute another tenant's alerts by raising a matching source.
func (i *Inhibitor) Inhibited(target *alert.Alert, firing []*alert.Alert) bool {
	if target == nil {
		return false
	}
	for _, r := range i.rules {
		if !silence.MatchAll(r.TargetMatchers, target.Labels) {
			continue
		}
		for _, src := range firing {
			if src == nil || src.Status != alert.StatusFiring {
				continue
			}
			if src.Fingerprint == target.Fingerprint {
				continue
			}
			if src.Tenant() != target.Tenant() {
				continue
			}
			if !silence.MatchAll(r.SourceMatchers, src.Labels) {
				continue
			}
			if equalLabels(r.Equal, src.Labels, target.Labels) {
				return true
			}
		}
	}
	return false
}

// equalLabels reports whether source and target carry the same value for every
// named label. A label absent from both reads as the empty string on each side
// and so counts as equal.
func equalLabels(names []string, source, target map[string]string) bool {
	for _, n := range names {
		if source[n] != target[n] {
			return false
		}
	}
	return true
}
