// Package rules implements the alerting rule model, a PromQL-lite expression
// language (lexer, recursive-descent parser, AST evaluator), the state machine
// that turns evaluation results into alerts, and the scheduler that evaluates
// rules on their own interval.
package rules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"metrics-system/internal/alerting/alert"
)

// ---------------------------------------------------------------------------
// Expression evaluation contract
// ---------------------------------------------------------------------------

// Sample is one labelled value produced by evaluating an expression.
type Sample struct {
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
}

// Vector is the result of evaluating an expression at an instant: zero or more
// samples, one per matching series. An alerting condition such as
// `cpu_usage_percent > 90` evaluates to the samples that satisfy it, so an
// empty vector means "nothing is wrong".
type Vector []Sample

// Point is a timestamped value inside a series.
type Point struct {
	T time.Time
	V float64
}

// Series is a labelled run of points, the input to range functions like
// rate() and avg_over_time().
type Series struct {
	Labels map[string]string
	Points []Point
}

// Querier is the read side of storage as the expression evaluator needs it.
// The implementation is responsible for tenant scoping: a rule owned by
// tenant-a must never observe tenant-b's series.
type Querier interface {
	// Instant returns the most recent sample of each matching series at or
	// before at (within an implementation-defined lookback window).
	Instant(ctx context.Context, name string, matchers map[string]string, at time.Time) (Vector, error)
	// Range returns the points of each matching series in [from, to].
	Range(ctx context.Context, name string, matchers map[string]string, from, to time.Time) ([]Series, error)
}

// Expression is a node of the parsed rule AST.
type Expression interface {
	// Eval evaluates the node at instant at.
	Eval(ctx context.Context, q Querier, at time.Time) (Vector, error)
	// String renders the node back to DSL text (used in errors and API output).
	String() string
}

// ---------------------------------------------------------------------------
// Rule model
// ---------------------------------------------------------------------------

// Severity ranks an alert for the humans who receive it.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// ParseSeverity validates a severity string, defaulting an empty one to warning.
func ParseSeverity(s string) (Severity, error) {
	switch Severity(strings.ToLower(strings.TrimSpace(s))) {
	case "":
		return SeverityWarning, nil
	case SeverityInfo:
		return SeverityInfo, nil
	case SeverityWarning:
		return SeverityWarning, nil
	case SeverityCritical:
		return SeverityCritical, nil
	default:
		return "", fmt.Errorf("unknown severity %q (want info|warning|critical)", s)
	}
}

// Duration is a time.Duration that marshals as a Go duration string ("5m"), so
// rule files and the REST API read naturally instead of carrying nanoseconds.
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts both "30s" and a raw nanosecond count.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("duration %q: %w", s, err)
		}
		*d = Duration(parsed)
		return nil
	}
	var n int64
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("duration: want a string like \"5m\" or a nanosecond count")
	}
	*d = Duration(n)
	return nil
}

// DefaultInterval is how often a rule is evaluated when it does not say.
const DefaultInterval = 15 * time.Second

// Rule is a condition evaluated on a schedule, plus the metadata attached to
// every alert it produces.
type Rule struct {
	ID          string            `json:"id"`
	TenantID    string            `json:"tenant_id"`
	Name        string            `json:"name"`
	Expression  string            `json:"expression"`
	For         Duration          `json:"for"`
	Interval    Duration          `json:"interval"`
	Severity    Severity          `json:"severity"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Receivers   []string          `json:"receivers,omitempty"`
	Enabled     bool              `json:"enabled"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`

	compiled Expression
}

// Compiled returns the parsed AST, or nil if Compile has not been called.
func (r *Rule) Compiled() Expression { return r.compiled }

// SetCompiled installs a pre-parsed AST (used by tests and by the preview API).
func (r *Rule) SetCompiled(e Expression) { r.compiled = e }

// Compile parses the rule's expression, applies defaults and validates the
// result. A rule that fails to compile is never scheduled.
func (r *Rule) Compile() error {
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("rule name is required")
	}
	if strings.TrimSpace(r.Expression) == "" {
		return errors.New("rule expression is required")
	}
	// `tenant` is server-controlled: it is the label every isolation check reads.
	// The evaluator re-stamps it anyway, but rejecting it here tells whoever wrote
	// the rule why their label vanished.
	if _, ok := r.Labels["tenant"]; ok {
		return errors.New(`"tenant" is a reserved label and cannot be set by a rule`)
	}
	expr, forDur, err := Parse(r.Expression)
	if err != nil {
		return fmt.Errorf("expression: %w", err)
	}
	// A `for` clause inside the expression sets the rule's For unless the rule
	// already carries an explicit one.
	if r.For == 0 && forDur > 0 {
		r.For = Duration(forDur)
	}
	if r.For < 0 {
		return errors.New("for must not be negative")
	}
	if r.Interval <= 0 {
		r.Interval = Duration(DefaultInterval)
	}
	sev, err := ParseSeverity(string(r.Severity))
	if err != nil {
		return err
	}
	r.Severity = sev
	r.compiled = expr
	return nil
}

// Clone deep-copies the rule (the AST is immutable and shared).
func (r *Rule) Clone() *Rule {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Labels = alert.CloneLabels(r.Labels)
	cp.Annotations = alert.CloneLabels(r.Annotations)
	if r.Receivers != nil {
		cp.Receivers = append([]string(nil), r.Receivers...)
	}
	return &cp
}

// ---------------------------------------------------------------------------
// Alert state machine
// ---------------------------------------------------------------------------

// State is where one label set of one rule sits in the alert lifecycle.
//
//	Inactive ──(condition true)──▶ Pending ──(For elapsed)──▶ Firing
//	    ▲                             │                          │
//	    └──────(condition false)──────┴──────────────────────────┘
//	                                                    (emits Resolved)
type State string

const (
	StateInactive State = "inactive" // condition not met
	StatePending  State = "pending"  // condition met, For has not elapsed yet
	StateFiring   State = "firing"   // condition met for at least For
	StateResolved State = "resolved" // was firing, condition has gone away
)

// AlertState is the evaluator's per-fingerprint memory across evaluations.
type AlertState struct {
	RuleID      string            `json:"rule_id"`
	Fingerprint string            `json:"fingerprint"`
	Labels      map[string]string `json:"labels"`
	Value       float64           `json:"value"`
	State       State             `json:"state"`
	ActiveAt    time.Time         `json:"active_at"` // when the condition first held
	FiredAt     *time.Time        `json:"fired_at,omitempty"`
	ResolvedAt  *time.Time        `json:"resolved_at,omitempty"`
	LastEvalAt  time.Time         `json:"last_eval_at"`
	LastSentAt  time.Time         `json:"last_sent_at,omitempty"`
}

// Clone deep-copies the state.
func (s *AlertState) Clone() *AlertState {
	if s == nil {
		return nil
	}
	cp := *s
	cp.Labels = alert.CloneLabels(s.Labels)
	if s.FiredAt != nil {
		t := *s.FiredAt
		cp.FiredAt = &t
	}
	if s.ResolvedAt != nil {
		t := *s.ResolvedAt
		cp.ResolvedAt = &t
	}
	return &cp
}
