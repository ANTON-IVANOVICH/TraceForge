package silence

import "testing"

// FuzzParseMatcher asserts ParseMatcher never panics and that a parsed matcher
// prints to a form that re-parses to an equal matcher. The round trip is the
// real target: String and ParseMatcher must be inverses, or a silence written,
// stored as text, and reloaded would mute a different set of alerts than the one
// its author selected. The compiled regex is derived state, so equality is over
// the (Name, Op, Value) triple only.
func FuzzParseMatcher(f *testing.F) {
	for _, s := range []string{
		`host="web-1"`,
		`env=~"prod.*"`,
		`x!="y"`,
		`x!~"y"`,
		`host=web-1`,
		`env=~\d+`,
		`k="a\"b"`,        // embedded quote — the escaping-vs-literal-strip stressor
		`k="back\\slash"`, // embedded backslash
		`k="a=b"`,         // '=' inside a value must not confuse the operator scan
		"k=\"line1\nline2\"",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, in string) {
		m, err := ParseMatcher(in) // must never panic
		if err != nil {
			return
		}
		printed := m.String()
		m2, err := ParseMatcher(printed)
		if err != nil {
			t.Fatalf("String() %q of matcher parsed from %q failed to re-parse: %v", printed, in, err)
		}
		if m2.Name != m.Name || m2.Op != m.Op || m2.Value != m.Value {
			t.Fatalf("round trip changed the matcher:\n in:      %q -> {name:%q op:%q value:%q}\n printed: %q -> {name:%q op:%q value:%q}",
				in, m.Name, m.Op, m.Value, printed, m2.Name, m2.Op, m2.Value)
		}
	})
}
