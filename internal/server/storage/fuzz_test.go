package storage

import (
	"maps"
	"testing"
)

// SeriesKey is the identity of a series everywhere in the system: the map key in
// MemoryStorage, the bbolt key, the chunk index key, the grouping key in the
// alerting querier. Two different (name, labels) pairs mapping to the same key
// means two different series silently merge into one — points from one show up
// in queries for the other, and nothing anywhere reports an error.
//
// So the property to fuzz is not "does not panic". It is injectivity:
//
//	SeriesKey(n1, l1) == SeriesKey(n2, l2)  <=>  n1 == n2 && l1 == l2
//
// A hand-written table of cases cannot establish that. A fuzzer, given the
// delimiters as raw material, finds a counterexample in under a second.

// decodeLabels turns fuzzer bytes into a label map with a length-prefixed
// format, which lets the fuzzer explore label sets rather than only strings.
// It is deliberately unambiguous — the very property SeriesKey lacked.
func decodeLabels(data []byte) (map[string]string, bool) {
	labels := make(map[string]string)
	for len(data) > 0 {
		if len(labels) >= 8 { // keep inputs small; the bug does not need more
			return nil, false
		}
		kLen := int(data[0])
		data = data[1:]
		if len(data) < kLen {
			return nil, false
		}
		key := string(data[:kLen])
		data = data[kLen:]
		if len(data) < 1 {
			return nil, false
		}
		vLen := int(data[0])
		data = data[1:]
		if len(data) < vLen {
			return nil, false
		}
		labels[key] = string(data[:vLen])
		data = data[vLen:]
	}
	return labels, true
}

func FuzzSeriesKeyInjective(f *testing.F) {
	// Ordinary series...
	f.Add("cpu", "cpu", []byte{}, []byte{})
	f.Add("cpu", "mem", []byte{1, 'a', 1, 'b'}, []byte{1, 'a', 1, 'c'})
	f.Add("http_requests_total", "http_requests_total",
		[]byte{4, 'h', 'o', 's', 't', 5, 'w', 'e', 'b', '-', '1'},
		[]byte{4, 'h', 'o', 's', 't', 5, 'w', 'e', 'b', '-', '2'})

	// ...and the three collision families the old, unescaped encoding admitted.
	// Seeds run on every `go test`, so these are regression tests that happen to
	// also be fuzz starting points.
	f.Add("cpu", "cpu", []byte{1, 'a', 3, 'b', ',', 'c'}, []byte{1, 'a', 1, 'b', 1, 'c', 1, 'd'}) // ',' in a value
	f.Add("cpu", "cpu", []byte{1, 'a', 3, 'b', '=', 'c'}, []byte{3, 'a', '=', 'b', 1, 'c'})       // '=' in a value
	f.Add("cpu{a=b}", "cpu", []byte{}, []byte{1, 'a', 1, 'b'})                                    // '{' in the name

	f.Fuzz(func(t *testing.T, name1, name2 string, raw1, raw2 []byte) {
		labels1, ok1 := decodeLabels(raw1)
		labels2, ok2 := decodeLabels(raw2)
		if !ok1 || !ok2 {
			t.Skip()
		}

		key1 := SeriesKey(name1, labels1)
		key2 := SeriesKey(name2, labels2)

		sameSeries := name1 == name2 && maps.Equal(labels1, labels2)
		sameKey := key1 == key2

		if sameKey != sameSeries {
			if sameKey {
				t.Fatalf("distinct series share a key %q:\n  (%q, %v)\n  (%q, %v)",
					key1, name1, labels1, name2, labels2)
			}
			t.Fatalf("identical series produced different keys %q and %q for (%q, %v)",
				key1, key2, name1, labels1)
		}
	})
}
func FuzzSeriesKeyRoundTrip(f *testing.F) {
	f.Add("cpu", []byte{})
	f.Add("cpu", []byte{1, 'a', 1, 'b'})
	f.Add("http_requests_total", []byte{4, 'h', 'o', 's', 't', 5, 'w', 'e', 'b', '-', '1'})
	f.Add("cpu", []byte{1, 'a', 3, 'b', ',', 'c'})
	f.Add(`weird\name{`, []byte{1, '=', 1, '}'})

	f.Fuzz(func(t *testing.T, name string, raw []byte) {
		labels, ok := decodeLabels(raw)
		if !ok {
			t.Skip()
		}

		key := SeriesKey(name, labels)

		gotName, gotLabels, err := ParseSeriesKey(key)
		if err != nil {
			t.Fatalf("SeriesKey produced an unparseable key %q for (%q, %v): %v", key, name, labels, err)
		}
		if gotName != name {
			t.Fatalf("name round-trip: want %q, got %q (key %q)", name, gotName, key)
		}
		if !maps.Equal(labels, gotLabels) && (len(labels) != 0 || len(gotLabels) != 0) {
			t.Fatalf("labels round-trip: want %v, got %v (key %q)", labels, gotLabels, key)
		}
	})
}
