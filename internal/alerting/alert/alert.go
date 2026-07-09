// Package alert holds the alert model that flows from rule evaluation to
// notification, plus the grouper that batches related alerts into the units a
// receiver actually delivers.
package alert

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"sort"
	"strings"
	"time"
)

// Status is where an alert sits in its lifecycle from the notifier's point of
// view. The finer-grained rule states (pending, inactive) never leave the
// evaluator: a notification is only ever "this is firing" or "this is over".
type Status string

const (
	StatusFiring   Status = "firing"
	StatusResolved Status = "resolved"
)

// Alert is one firing (or resolved) instance of a rule for one label set.
type Alert struct {
	Fingerprint string            `json:"fingerprint"`
	RuleID      string            `json:"rule_id"`
	RuleName    string            `json:"rule_name"`
	Status      Status            `json:"status"`
	Severity    string            `json:"severity"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations,omitempty"`
	StartsAt    time.Time         `json:"starts_at"`
	EndsAt      *time.Time        `json:"ends_at,omitempty"`
	Value       float64           `json:"value"`

	// Receivers routes this alert; it comes from the rule, not from the wire.
	Receivers []string `json:"receivers,omitempty"`
}

// Tenant is the isolation boundary the alert belongs to, carried as a
// server-controlled label (empty when auth is off).
func (a *Alert) Tenant() string {
	if a == nil {
		return ""
	}
	return a.Labels["tenant"]
}

// Name is the human-facing alert name, used for grouping and display.
func (a *Alert) Name() string {
	if a == nil {
		return ""
	}
	if n := a.Labels["alertname"]; n != "" {
		return n
	}
	return a.RuleName
}

// Clone deep-copies the alert so a receiver can hold on to it while the
// evaluator keeps mutating its own state.
func (a *Alert) Clone() *Alert {
	if a == nil {
		return nil
	}
	cp := *a
	cp.Labels = CloneLabels(a.Labels)
	cp.Annotations = CloneLabels(a.Annotations)
	if a.EndsAt != nil {
		t := *a.EndsAt
		cp.EndsAt = &t
	}
	if a.Receivers != nil {
		cp.Receivers = append([]string(nil), a.Receivers...)
	}
	return &cp
}

// Group is a batch of alerts sharing the same group-by labels, destined for one
// receiver. It is the unit a Receiver delivers: one email about fifty hosts,
// not fifty emails.
type Group struct {
	Key       string            `json:"key"`
	Receiver  string            `json:"receiver"`
	Labels    map[string]string `json:"labels"`
	Alerts    []*Alert          `json:"alerts"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// Status reports firing if any alert in the group is still firing.
func (g *Group) Status() Status {
	for _, a := range g.Alerts {
		if a.Status == StatusFiring {
			return StatusFiring
		}
	}
	return StatusResolved
}

// Counts returns how many alerts are firing and how many are resolved.
func (g *Group) Counts() (firing, resolved int) {
	for _, a := range g.Alerts {
		if a.Status == StatusFiring {
			firing++
		} else {
			resolved++
		}
	}
	return firing, resolved
}

// CloneLabels copies a label map (nil-safe).
func CloneLabels(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// MergeLabels overlays each map onto the previous one; later maps win.
func MergeLabels(maps ...map[string]string) map[string]string {
	out := make(map[string]string)
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// LabelsString renders labels deterministically, e.g. `host="web-1", tenant="a"`.
func LabelsString(labels map[string]string) string {
	if len(labels) == 0 {
		return "{}"
	}
	keys := sortedKeys(labels)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+`="`+labels[k]+`"`)
	}
	return strings.Join(parts, ", ")
}

// Fingerprint is the stable identity of an alert: the rule it came from plus
// its label set. Label keys are sorted before hashing, so the same alert keeps
// the same fingerprint across evaluations, restarts and map iteration orders —
// without that, dedup breaks and every evaluation re-notifies.
func Fingerprint(ruleID string, labels map[string]string) string {
	h := sha256.New()
	hashField(h, ruleID)
	for _, k := range sortedKeys(labels) {
		hashField(h, k)
		hashField(h, labels[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// hashField feeds s to h behind an explicit length prefix so field boundaries
// are unambiguous. A plain key='='+value+NUL join collided whenever a key held
// an '=' or a value spanned a separator: {"a=b":"c"} and {"a":"b=c"} produced
// the same bytes and thus the same fingerprint, so one alert silently took over
// the other's identity in dedup and grouping. Length-prefixing makes the encoding
// injective, which is the property a fingerprint actually needs.
func hashField(h hash.Hash, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	h.Write(n[:])
	h.Write([]byte(s))
}

// SortAlerts orders alerts by fingerprint so group payloads are deterministic.
func SortAlerts(as []*Alert) {
	sort.Slice(as, func(i, j int) bool { return as[i].Fingerprint < as[j].Fingerprint })
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
