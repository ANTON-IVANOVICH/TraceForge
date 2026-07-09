package rules

import (
	"strings"
	"testing"
	"time"
)

func TestParseRoundTrip(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"cpu_usage_percent > 90",
		"mem_free_bytes < 100 * 1024 * 1024",
		"rate(http_requests_total[1m]) > 1000",
		"avg_over_time(cpu_usage_percent[10m]) > 75",
		"max by (agent_id) (disk_used_percent) > 85",
		"sum without (agent_id) (disk_used_bytes)",
		"avg(cpu_usage_percent) by (tenant)",
		`up{env=~"prod.*", host!="web-1"} == 0`,
		"-cpu_usage_percent",
		"(cpu_usage_percent > 90) and (memory_used_percent > 80)",
		"cpu_usage_percent > 90 or memory_used_percent > 90",
		"cpu_usage_percent > 90 unless maintenance_mode == 1",
		"abs(delta(temperature[5m])) > 10",
		"clamp_min(cpu_usage_percent, 0) > 50",
		"count(up == 0) > 3",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			expr, _, err := Parse(in)
			if err != nil {
				t.Fatalf("Parse(%q) failed: %v", in, err)
			}
			once := expr.String()

			// The printed form must parse back to an identical tree, which proves
			// String() is a faithful rendering rather than a lossy approximation.
			expr2, _, err := Parse(once)
			if err != nil {
				t.Fatalf("re-parsing %q failed: %v", once, err)
			}
			if twice := expr2.String(); twice != once {
				t.Fatalf("round trip is not stable:\n first: %s\nsecond: %s", once, twice)
			}
		})
	}
}

func TestParseForClause(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		input string
		want  time.Duration
	}{
		"absent":            {"cpu > 90", 0},
		"minutes":           {"cpu > 90 for 5m", 5 * time.Minute},
		"compound":          {"cpu > 90 for 1h30m", 90 * time.Minute},
		"after aggregation": {"max by (host) (cpu) > 90 for 2m", 2 * time.Minute},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, forDur, err := Parse(tc.input)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.input, err)
			}
			if forDur != tc.want {
				t.Fatalf("for = %v, want %v", forDur, tc.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		input   string
		wantSub string
	}{
		"trailing operator":     {"cpu >", "expected an expression"},
		"unbalanced paren":      {"(cpu > 90", `expected ")"`},
		"unknown function":      {"nope(cpu) > 1", `unknown function "nope"`},
		"rate without range":    {"rate(cpu) > 1", "requires a range vector selector"},
		"instant fn arity":      {"abs(1, 2)", "expects 1 argument"},
		"range fn arity":        {"rate(cpu[1m], 2)", "expects exactly one argument"},
		"bad duration in range": {"rate(cpu[abc]) > 1", "expected a range duration"},
		"zero range":            {"rate(cpu[0s]) > 1", "expected a range duration"},
		"chained comparison":    {"a > b > c", "expected end of input"},
		"bad regex":             {`up{env=~"("} == 0`, "invalid regular expression"},
		"unquoted matcher":      {"up{env=prod} == 0", "expected a quoted string"},
		"unterminated string":   {`up{env="prod} == 0`, "unterminated string"},
		"for without duration":  {"cpu > 90 for", `expected a duration after "for"`},
		"bare duration":         {"5m", "expected an expression"},
		"empty":                 {"", "expected an expression"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := Parse(tc.input)
			if err == nil {
				t.Fatalf("Parse(%q) unexpectedly succeeded", tc.input)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Parse(%q) error = %q, want it to contain %q", tc.input, err, tc.wantSub)
			}
		})
	}
}

// `and` and `unless` bind tighter than `or`, as in PromQL. Getting this wrong
// silently changes which alerts fire.
func TestOperatorPrecedence(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"a or b unless c": "(a or (b unless c))",
		"a or b and c":    "(a or (b and c))",
		"a unless b or c": "((a unless b) or c)",
		"a and b or c":    "((a and b) or c)",
		"1 + 2 * 3":       "(1 + (2 * 3))",
		"1 * 2 + 3":       "((1 * 2) + 3)",
		"-1 + 2":          "(-1 + 2)",
		"a > 1 and b > 2": "((a > 1) and (b > 2))",
		"1 - 2 - 3":       "((1 - 2) - 3)", // arithmetic is left-associative
	}
	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			expr, _, err := Parse(input)
			if err != nil {
				t.Fatalf("Parse(%q): %v", input, err)
			}
			if got := expr.String(); got != want {
				t.Fatalf("Parse(%q) = %s, want %s", input, got, want)
			}
		})
	}
}

// A pathological input must produce an error, never a stack overflow.
func TestParseRejectsDeepNesting(t *testing.T) {
	t.Parallel()
	deep := strings.Repeat("(", 200) + "1" + strings.Repeat(")", 200)
	if _, _, err := Parse(deep); err == nil {
		t.Fatal("deeply nested expression parsed without error")
	} else if !strings.Contains(err.Error(), "nested too deeply") {
		t.Fatalf("error = %q, want a depth-limit error", err)
	}
}

func TestParseRejectsOversizedInput(t *testing.T) {
	t.Parallel()
	if _, _, err := Parse(strings.Repeat("a", maxInputLen+1)); err == nil {
		t.Fatal("oversized expression parsed without error")
	}
}

func TestParseRejectsOversizedRegex(t *testing.T) {
	t.Parallel()
	pattern := strings.Repeat("a", maxPatternLen+1)
	if _, _, err := Parse(`up{env=~"` + pattern + `"} == 0`); err == nil {
		t.Fatal("oversized regex parsed without error")
	}
}

// Errors must point at the offending byte so a user can find their typo.
func TestParseErrorCarriesPosition(t *testing.T) {
	t.Parallel()
	_, _, err := Parse("cpu_usage_percent > ")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "position 20") {
		t.Fatalf("error = %q, want it to report position 20", err)
	}
}

// Regex matchers are fully anchored, so =~"prod" must not match "production".
func TestLabelMatcherRegexIsAnchored(t *testing.T) {
	t.Parallel()
	expr, _, err := Parse(`up{env=~"prod"} == 0`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	bin, ok := expr.(*BinaryOp)
	if !ok {
		t.Fatalf("expected a BinaryOp, got %T", expr)
	}
	ref := bin.Left.(*MetricRef)
	m := ref.Matchers[0]
	if !m.matches("prod") {
		t.Error(`=~"prod" should match "prod"`)
	}
	if m.matches("production") {
		t.Error(`=~"prod" must not match "production" — the pattern is anchored`)
	}
}
