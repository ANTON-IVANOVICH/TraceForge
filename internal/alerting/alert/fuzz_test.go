package alert

import (
	"maps"
	"testing"
)

// Unit and record separators frame the fuzzer's encoded label maps. Because
// they are distinct from '=', the fuzzer can put an '=' anywhere in a key or
// value — which is exactly the input class that exposed the old framing
// collision. Keys and values may still contain the separators; that just yields
// a different decoded map, which is fine since the map is the ground truth.
const (
	unitSep   = '\x1f'
	recordSep = '\x1e'
)

// decodeLabels turns fuzzer bytes into a label map. Each record is one label;
// the first unit separator splits key from value (a value may contain more).
func decodeLabels(enc string) map[string]string {
	m := map[string]string{}
	if enc == "" {
		return m
	}
	start := 0
	flush := func(rec string) {
		key, val := rec, ""
		for i := 0; i < len(rec); i++ {
			if rec[i] == unitSep {
				key, val = rec[:i], rec[i+1:]
				break
			}
		}
		m[key] = val
	}
	for i := 0; i < len(enc); i++ {
		if enc[i] == recordSep {
			flush(enc[start:i])
			start = i + 1
		}
	}
	flush(enc[start:])
	return m
}

// FuzzFingerprintDistinct is the collision hunt: two different (ruleID, labels)
// inputs must never share a fingerprint. Decoding both pairs inside one call
// makes a collision a self-contained failure — the fuzzer does not need to
// rediscover a colliding partner across runs, it only needs to generate the two
// halves once. The seeds include the exact witness that used to collide.
func FuzzFingerprintDistinct(f *testing.F) {
	// The canonical collision: {"a=b":"c"} vs {"a":"b=c"} under the same ruleID.
	f.Add("r", "a=b\x1fc", "r", "a\x1fb=c")
	f.Add("rule-1", "host\x1fweb-1\x1etenant\x1facme", "rule-1", "host\x1fweb-2\x1etenant\x1facme")
	f.Add("a", "", "b", "")
	f.Add("r", "k\x1f", "r", "\x1fk")

	f.Fuzz(func(t *testing.T, id1, enc1, id2, enc2 string) {
		l1 := decodeLabels(enc1)
		l2 := decodeLabels(enc2)

		same := id1 == id2 && maps.Equal(l1, l2)
		fp1 := Fingerprint(id1, l1)
		fp2 := Fingerprint(id2, l2)

		if !same && fp1 == fp2 {
			t.Fatalf("fingerprint collision: (%q, %v) and (%q, %v) both hash to %s",
				id1, l1, id2, l2, fp1)
		}
		if same && fp1 != fp2 {
			t.Fatalf("fingerprint not deterministic for %q %v: %s != %s", id1, l1, fp1, fp2)
		}
	})
}
