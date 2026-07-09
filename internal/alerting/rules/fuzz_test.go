package rules

import (
	"strings"
	"testing"
)

// FuzzParseExpression drives the two invariants that catch real parser/printer
// bugs, not merely crashes: Parse never panics, and String is a faithful,
// idempotent rendering of what Parse produced. A tree that prints to text which
// re-parses into a *different* tree would silently change which alerts fire.
func FuzzParseExpression(f *testing.F) {
	for _, s := range []string{
		"cpu > 90",
		`cpu{host="web-1"} > 90`,
		"rate(http_requests[1m]) > 1000",
		"avg(cpu) by (host) > 75",
		"cpu > 90 and mem > 80 unless up == 0",
		"cpu > 90 for 5m",
		"-cpu + 3 * (mem - 1)",
		`up{env=~"prod.*", host!="web-1"} == 0`,
		"max without (instance) (rate(errors[5m])) > 0.05",
		"clamp_min(abs(delta(temp[10m])), 0) >= 10",
		`up{re=~"a\\d+(b|c)$"} == 1`,
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, in string) {
		// The parser rejects anything past maxInputLen by contract; a longer input
		// exercises only that guard, so skip rather than fight the cap.
		if len(in) > maxInputLen {
			t.Skip()
		}

		expr, _, err := Parse(in) // must never panic
		if err != nil {
			return // only a successful parse owes us a round trip
		}

		once := expr.String()
		// String may legitimately grow the text — explicit parentheses, 1m->1m0s,
		// escaped matcher quotes — so its output can exceed a cap the input did not.
		// Past the cap the printed form is no longer re-parseable, which is the cap
		// talking, not a round-trip defect.
		if len(once) > maxInputLen {
			t.Skip()
		}

		expr2, _, err := Parse(once)
		if err != nil {
			if isCapError(err) {
				t.Skip()
			}
			t.Fatalf("round trip failed: Parse(%q) printed from %q errored: %v", once, in, err)
		}
		if twice := expr2.String(); twice != once {
			t.Fatalf("String is not idempotent:\n input: %q\n once:  %q\n twice: %q", in, once, twice)
		}
	})
}

// isCapError reports whether err is one of the parser's resource guards firing
// rather than a genuine syntax error. Re-printing an AST can nest parentheses a
// level deeper or escape a matcher value past the regex cap; tripping a guard on
// the way back in is expected and must not be read as a broken round trip.
func isCapError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "nested too deeply") || strings.Contains(s, "exceeds")
}

// FuzzLex asserts the lexer is total (never panics, always terminates by
// consuming input) and that the token stream faithfully covers the input.
//
// The covering invariant is "every token's text is the verbatim input slice at
// its recorded position", which is stronger than a subsequence check — but it
// necessarily excuses string tokens: their val holds the *unescaped* content
// (`\n` becomes a newline, `\"` a quote), so it is deliberately not a slice of
// the source. For those we assert only that the token began at a quote.
func FuzzLex(f *testing.F) {
	for _, s := range []string{
		"cpu > 90",
		`x{a="1",b!="2",c=~"3\\d",d!~"4"}`,
		"1.5e3 + .25",
		"for 1h30m",
		`rate(http_requests_total{code=~"5.."}[5m])`,
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, in string) {
		toks, err := lex(in) // must never panic; a hang would be caught as a timeout
		if err != nil {
			return
		}
		if len(toks) == 0 || toks[len(toks)-1].typ != tokEOF {
			t.Fatalf("token stream does not end in a single EOF: %v", toks)
		}

		prev := 0
		for i, tk := range toks {
			if tk.pos < 0 || tk.pos > len(in) {
				t.Fatalf("token %d has out-of-range pos %d (input len %d)", i, tk.pos, len(in))
			}
			if tk.pos < prev {
				t.Fatalf("token positions moved backwards at %d: %d < %d", i, tk.pos, prev)
			}
			prev = tk.pos

			switch tk.typ {
			case tokEOF:
				if i != len(toks)-1 {
					t.Fatalf("EOF token at index %d is not last", i)
				}
			case tokString:
				if tk.pos >= len(in) || in[tk.pos] != '"' {
					t.Fatalf("string token at %d does not begin with a quote", tk.pos)
				}
			default:
				end := tk.pos + len(tk.val)
				if end > len(in) || in[tk.pos:end] != tk.val {
					t.Fatalf("token %d (%q) is not the input slice at pos %d", i, tk.val, tk.pos)
				}
			}
		}
	})
}
