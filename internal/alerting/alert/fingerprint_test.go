package alert

import "testing"

// Fingerprint framing must be injective: distinct (ruleID, labels) inputs must
// never hash to the same value. The pre-fix join wrote key + '=' + value + NUL,
// which was ambiguous whenever a key contained '=' or a value spanned the
// separator — so two genuinely different alerts collided and one silently
// suppressed the other in the dedup and grouping paths.
//
// Each pair below is a witness that used to collide; they must now stay distinct.
func TestFingerprintNoFramingCollision(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		aID, bID   string
		aLbl, bLbl map[string]string
	}{
		{
			// The canonical witness: an '=' inside a key is indistinguishable from
			// the '=' between a key and value once the join drops length framing.
			// Both used to hash to c85ec21a81727288.
			name: "equals in key vs value",
			aID:  "r", aLbl: map[string]string{"a=b": "c"},
			bID: "r", bLbl: map[string]string{"a": "b=c"},
		},
		{
			// The same shift one pair over: the NUL terminator lines up, so the '='
			// ambiguity survives into a multi-label alert.
			name: "equals shift across pairs",
			aID:  "r", aLbl: map[string]string{"a": "b", "c=d": "e"},
			bID: "r", bLbl: map[string]string{"a": "b", "c": "d=e"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a := Fingerprint(c.aID, c.aLbl)
			b := Fingerprint(c.bID, c.bLbl)
			if a == b {
				t.Fatalf("collision: (%q,%v) and (%q,%v) both fingerprint to %s",
					c.aID, c.aLbl, c.bID, c.bLbl, a)
			}
		})
	}
}

// The property the notifier depends on: the same input always yields the same
// fingerprint regardless of map iteration order.
func TestFingerprintDeterministic(t *testing.T) {
	t.Parallel()
	want := Fingerprint("r", map[string]string{"host": "a", "tenant": "x", "job": "n"})
	for i := 0; i < 100; i++ {
		got := Fingerprint("r", map[string]string{"job": "n", "tenant": "x", "host": "a"})
		if got != want {
			t.Fatalf("fingerprint not stable across iterations: %s != %s", got, want)
		}
	}
}
