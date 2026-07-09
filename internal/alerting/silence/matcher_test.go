package silence

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMatcherMatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		op     MatchOp
		value  string
		labels map[string]string
		want   bool
	}{
		{"host", MatchEqual, "web-1", map[string]string{"host": "web-1"}, true},
		{"host", MatchEqual, "web-1", map[string]string{"host": "web-2"}, false},
		{"host", MatchNotEqual, "web-1", map[string]string{"host": "web-2"}, true},
		{"host", MatchNotEqual, "web-1", map[string]string{"host": "web-1"}, false},
		{"env", MatchRegexp, "prod.*", map[string]string{"env": "production"}, true},
		{"env", MatchRegexp, "prod.*", map[string]string{"env": "staging"}, false},
		{"env", MatchNotRegexp, "prod.*", map[string]string{"env": "staging"}, true},
		// Missing label reads as the empty string.
		{"x", MatchEqual, "", map[string]string{}, true},
		{"x", MatchNotRegexp, ".+", map[string]string{}, true},
		{"x", MatchNotRegexp, ".+", map[string]string{"x": "y"}, false},
		{"x", MatchRegexp, ".+", map[string]string{}, false},
	}
	for _, c := range cases {
		m, err := NewMatcher(c.name, c.op, c.value)
		if err != nil {
			t.Fatalf("NewMatcher(%s%s%q): %v", c.name, c.op, c.value, err)
		}
		if got := m.Match(c.labels); got != c.want {
			t.Errorf("%s%s%q against %v: got %v want %v", c.name, c.op, c.value, c.labels, got, c.want)
		}
	}
}

func TestMatcherAnchoring(t *testing.T) {
	t.Parallel()
	// Prometheus semantics: `env=~"prod"` matches exactly "prod", not "production".
	m, err := NewMatcher("env", MatchRegexp, "prod")
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	if m.Match(map[string]string{"env": "production"}) {
		t.Error(`=~"prod" wrongly matched "production"`)
	}
	if !m.Match(map[string]string{"env": "prod"}) {
		t.Error(`=~"prod" should match "prod"`)
	}
}

func TestNewMatcherErrors(t *testing.T) {
	t.Parallel()
	if _, err := NewMatcher("x", MatchRegexp, "("); err == nil {
		t.Error("invalid regex should be rejected")
	}
	if _, err := NewMatcher("x", MatchOp("~~"), "y"); err == nil {
		t.Error("unknown operator should be rejected")
	}
	if _, err := NewMatcher("x", MatchRegexp, strings.Repeat("a", maxPatternLen+1)); err == nil {
		t.Error("oversized pattern should be rejected")
	}
	if _, err := NewMatcher("x", MatchRegexp, strings.Repeat("a", maxPatternLen)); err != nil {
		t.Errorf("pattern at the cap should be accepted: %v", err)
	}
}

func TestParseMatcher(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		name    string
		op      MatchOp
		value   string
		wantErr bool
	}{
		{`host="web-1"`, "host", MatchEqual, "web-1", false},
		{`env=~"prod.*"`, "env", MatchRegexp, "prod.*", false},
		{`x!="y"`, "x", MatchNotEqual, "y", false},
		{`x!~"y"`, "x", MatchNotRegexp, "y", false},
		{`host=web-1`, "host", MatchEqual, "web-1", false},           // quotes optional
		{`  host  =  "web-1"  `, "host", MatchEqual, "web-1", false}, // whitespace tolerated
		{`x=""`, "x", MatchEqual, "", false},
		{`env=~\d+`, "env", MatchRegexp, `\d+`, false}, // unquoted regex keeps backslashes
		{`noop`, "", "", "", true},
		{`="y"`, "", "", "", true},
	}
	for _, c := range cases {
		m, err := ParseMatcher(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseMatcher(%q): want error, got %+v", c.in, m)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMatcher(%q): %v", c.in, err)
			continue
		}
		if m.Name != c.name || m.Op != c.op || m.Value != c.value {
			t.Errorf("ParseMatcher(%q): got {%q %q %q} want {%q %q %q}",
				c.in, m.Name, m.Op, m.Value, c.name, c.op, c.value)
		}
	}
}

func TestMatcherString(t *testing.T) {
	t.Parallel()
	m, err := NewMatcher("host", MatchEqual, "web-1")
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	if got := m.String(); got != `host="web-1"` {
		t.Errorf("String() = %q want `host=\"web-1\"`", got)
	}
}

func TestMatcherJSONRecompiles(t *testing.T) {
	t.Parallel()
	m, err := NewMatcher("env", MatchRegexp, "prod.*")
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"op":"=~"`) {
		t.Errorf("marshalled form missing op field: %s", data)
	}

	var back Matcher
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Name != "env" || back.Op != MatchRegexp || back.Value != "prod.*" {
		t.Errorf("round-trip changed fields: %+v", back)
	}
	// The whole point of a custom UnmarshalJSON: the regex is usable again.
	if !back.Match(map[string]string{"env": "production"}) {
		t.Error("regex was not recompiled on unmarshal")
	}
}

func TestMatchAll(t *testing.T) {
	t.Parallel()
	labels := map[string]string{"host": "web-1", "env": "prod"}

	if MatchAll(nil, labels) {
		t.Error("empty matcher list must match nothing")
	}
	if MatchAll([]Matcher{}, labels) {
		t.Error("empty matcher list must match nothing")
	}

	host, _ := NewMatcher("host", MatchEqual, "web-1")
	env, _ := NewMatcher("env", MatchRegexp, "prod")
	if !MatchAll([]Matcher{host, env}, labels) {
		t.Error("all matchers satisfied, want true")
	}

	other, _ := NewMatcher("host", MatchEqual, "web-2")
	if MatchAll([]Matcher{host, other}, labels) {
		t.Error("one matcher unsatisfied, want false")
	}
}
