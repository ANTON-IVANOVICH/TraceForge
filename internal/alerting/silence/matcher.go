// Package silence mutes alerts for a time window (like Alertmanager silences)
// and provides the label matcher that silences, inhibition rules and routing
// all share. A matcher is one predicate over an alert's label set; a list of
// matchers is ANDed together, and an empty list deliberately matches nothing.
package silence

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// MatchOp is a label-matching operator, spelled as it appears in the DSL.
type MatchOp string

const (
	MatchEqual     MatchOp = "="  // label value equals Value
	MatchNotEqual  MatchOp = "!=" // label value differs from Value
	MatchRegexp    MatchOp = "=~" // label value matches the anchored regex Value
	MatchNotRegexp MatchOp = "!~" // label value does not match the anchored regex Value
)

// maxPatternLen caps a regex source so a hostile or fat-fingered silence cannot
// hand the regexp engine a pathological pattern to compile and run on every alert.
const maxPatternLen = 512

// Matcher is one predicate over a label set. For the regex operators the pattern
// is compiled and fully anchored once at construction; re is nil for the plain
// equality operators. A Matcher is immutable after construction and safe to share
// across goroutines (regexp.Regexp is itself concurrency-safe).
type Matcher struct {
	Name  string  `json:"name"`
	Op    MatchOp `json:"op"`
	Value string  `json:"value"`
	re    *regexp.Regexp
}

// NewMatcher compiles a matcher, anchoring regex patterns so they behave like
// Prometheus matchers: `env=~"prod"` matches exactly "prod", never "production".
func NewMatcher(name string, op MatchOp, value string) (Matcher, error) {
	m := Matcher{Name: name, Op: op, Value: value}
	switch op {
	case MatchEqual, MatchNotEqual:
		return m, nil
	case MatchRegexp, MatchNotRegexp:
		if len(value) > maxPatternLen {
			return Matcher{}, fmt.Errorf("matcher %q: pattern of %d bytes exceeds %d-byte cap", name, len(value), maxPatternLen)
		}
		// Full anchoring is the whole point: an unanchored regex is a substring
		// match, which silently over-matches and mutes alerts nobody intended.
		re, err := regexp.Compile("^(?:" + value + ")$")
		if err != nil {
			return Matcher{}, fmt.Errorf("matcher %q: invalid regex %q: %w", name, value, err)
		}
		m.re = re
		return m, nil
	default:
		return Matcher{}, fmt.Errorf("matcher %q: unknown operator %q", name, op)
	}
}

// ParseMatcher parses a matcher from its text form: `host="web-1"`,
// `env=~"prod.*"`, `x!="y"`. The surrounding double quotes are optional and are
// stripped literally, so regex backslashes survive unmangled.
func ParseMatcher(s string) (Matcher, error) {
	s = strings.TrimSpace(s)
	// Operators are the only place '=' or '!' can appear, so the first such byte
	// is where the label name ends and the operator begins.
	i := strings.IndexAny(s, "=!")
	if i < 0 {
		return Matcher{}, fmt.Errorf("matcher %q: no operator (want =, !=, =~ or !~)", s)
	}
	name := strings.TrimSpace(s[:i])
	rest := s[i:]

	var op MatchOp
	switch {
	case strings.HasPrefix(rest, "=~"):
		op, rest = MatchRegexp, rest[2:]
	case strings.HasPrefix(rest, "!~"):
		op, rest = MatchNotRegexp, rest[2:]
	case strings.HasPrefix(rest, "!="):
		op, rest = MatchNotEqual, rest[2:]
	case strings.HasPrefix(rest, "="):
		op, rest = MatchEqual, rest[1:]
	default:
		return Matcher{}, fmt.Errorf("matcher %q: invalid operator", s)
	}
	if name == "" {
		return Matcher{}, fmt.Errorf("matcher %q: empty label name", s)
	}

	value := strings.TrimSpace(rest)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	return NewMatcher(name, op, value)
}

// Match reports whether the matcher accepts labels. A missing label is treated
// as the empty string, so `x=""` and `x!~".+"` both accept a series that has no
// label x at all.
func (m Matcher) Match(labels map[string]string) bool {
	v := labels[m.Name]
	switch m.Op {
	case MatchEqual:
		return v == m.Value
	case MatchNotEqual:
		return v != m.Value
	case MatchRegexp:
		return m.re != nil && m.re.MatchString(v)
	case MatchNotRegexp:
		// A nil re means the matcher was never compiled; fail closed rather than
		// treat every value as "does not match" and over-mute.
		return m.re != nil && !m.re.MatchString(v)
	default:
		return false
	}
}

// String renders the matcher back to its text form, e.g. `host="web-1"`.
//
// The value is wrapped in quotes literally, never with %q: ParseMatcher strips
// the surrounding quotes without interpreting escapes (so regex backslashes
// survive), so an escaping printer would not round-trip — `host="a\"b"` would
// re-parse to the value `a\"b` rather than `a"b`. Literal quoting is the exact
// inverse of that literal stripping. The outer quotes remain recoverable even
// when the value itself contains a quote, because ParseMatcher only removes one
// quote from each end.
func (m Matcher) String() string {
	return m.Name + string(m.Op) + `"` + m.Value + `"`
}

// valid reports whether the matcher is usable: a known operator and, for the
// regex operators, a compiled pattern. It backstops silences hand-built from
// struct literals that skipped NewMatcher.
func (m Matcher) valid() error {
	switch m.Op {
	case MatchEqual, MatchNotEqual:
		return nil
	case MatchRegexp, MatchNotRegexp:
		if m.re == nil {
			return fmt.Errorf("matcher %q: regex not compiled (build it with NewMatcher)", m.Name)
		}
		return nil
	default:
		return fmt.Errorf("matcher %q: unknown operator %q", m.Name, m.Op)
	}
}

// MarshalJSON emits the matcher's public fields; the compiled regex is derived
// state and is rebuilt on unmarshal rather than stored.
func (m Matcher) MarshalJSON() ([]byte, error) {
	type wire Matcher // sheds the MarshalJSON method to avoid infinite recursion
	return json.Marshal(wire(m))
}

// UnmarshalJSON decodes the public fields and recompiles the regex, so a matcher
// read from storage or the wire is immediately usable — without this a decoded
// `=~` matcher would carry a nil regexp and silently match nothing.
func (m *Matcher) UnmarshalJSON(data []byte) error {
	type wire Matcher
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	nm, err := NewMatcher(w.Name, w.Op, w.Value)
	if err != nil {
		return err
	}
	*m = nm
	return nil
}

// MatchAll reports whether every matcher accepts labels. An empty matcher list
// returns false on purpose: a silence with no matchers would otherwise mute every
// alert in the system, so callers must treat "no matchers" as a validation error,
// not as "match everything".
func MatchAll(ms []Matcher, labels map[string]string) bool {
	if len(ms) == 0 {
		return false
	}
	for _, m := range ms {
		if !m.Match(labels) {
			return false
		}
	}
	return true
}
