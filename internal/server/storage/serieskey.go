package storage

import (
	"fmt"
	"slices"
	"strings"
)

// A series key is the identity of a series everywhere in the system: the map key
// in MemoryStorage, the bbolt key, the chunk index key, the grouping key in the
// alerting querier. Its one hard requirement is injectivity — distinct
// (name, labels) pairs must produce distinct keys. Otherwise two series merge
// silently: points written to one are returned by queries for the other, the
// stored label set is whichever writer arrived first, and nothing reports an
// error anywhere.
//
// The obvious encoding, name{k1=v1,k2=v2}, is not injective, because label keys
// and values may themselves contain the delimiters:
//
//	SeriesKey("cpu", map[string]string{"a": "b,c=d"})           == "cpu{a=b,c=d}"
//	SeriesKey("cpu", map[string]string{"a": "b", "c": "d"})     == "cpu{a=b,c=d}"
//	SeriesKey("cpu{a=b}", nil)                                  == "cpu{a=b}"
//
// So every delimiter is escaped with a backslash, and the backslash itself is
// escaped. That makes the encoding invertible, and ParseSeriesKey is the proof:
// a format you can decode cannot collide, because a collision would give one
// input two decodings.
//
// The escape is a no-op for a key that contains no delimiter, which is every key
// a well-behaved agent produces — so existing data keeps its existing keys, and
// only the keys that were previously colliding change shape.
const (
	keyOpen  = '{'
	keyClose = '}'
	keySep   = ','
	keyEq    = '='
	keyEsc   = '\\'
)

// keySpecial lists the bytes that carry structure and therefore must be escaped
// inside a name, a label key or a label value.
const keySpecial = `\,={}`

// isSpecial is a 256-entry lookup table rather than a call to strings.IndexByte.
// SeriesKey inspects every byte of every label of every metric written, and a
// function call per byte costs more than the escaping itself: replacing
// IndexByte/IndexAny with this table is what took the escaped encoder from three
// times slower than the unescaped one it replaced to faster than it.
var isSpecial = func() (t [256]bool) {
	for i := 0; i < len(keySpecial); i++ {
		t[keySpecial[i]] = true
	}
	return t
}()

// inlineSortMax is the label count below which the sort scratch space lives on
// the stack. Eight covers essentially every real series: Prometheus's own
// exposition guidance is to keep cardinality low, and the metrics this system
// collects carry two or three labels.
const inlineSortMax = 8

// SeriesKey canonicalizes a metric into a stable, injective key. Labels are
// sorted, so {host=a,region=b} and {region=b,host=a} collapse to one series.
func SeriesKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return escape(name)
	}

	// Collect key and value together in the single range over the map. Ranging
	// over keys and looking each value up again doubles the number of string
	// hashes, and map lookups — not the escaping — dominate this function.
	//
	// The scratch array keeps that slice on the stack for the common case; a
	// heap allocation here would be paid on every metric written.
	var inline [inlineSortMax]labelPair
	var pairs []labelPair
	if len(labels) <= inlineSortMax {
		pairs = inline[:0]
	} else {
		pairs = make([]labelPair, 0, len(labels))
	}
	for k, v := range labels {
		pairs = append(pairs, labelPair{k, v})
	}
	slices.SortFunc(pairs, func(a, b labelPair) int { return strings.Compare(a.key, b.key) })

	// Size the buffer up front so strings.Builder allocates once: '{' + '}' + one
	// '=' per label + one ',' between labels.
	//
	// The size assumes nothing needs escaping, which is exact for every key a
	// well-behaved agent produces. Pre-scanning for escapes to get an exact size
	// would double the bytes touched to save a single realloc on a path that is
	// almost never taken; when a label really does contain a delimiter, the
	// builder grows once and that is the whole cost.
	size := len(name) + 2*len(pairs) + 1
	for _, p := range pairs {
		size += len(p.key) + len(p.value)
	}

	var b strings.Builder
	b.Grow(size)
	writeEscaped(&b, name)
	b.WriteByte(keyOpen)
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(keySep)
		}
		writeEscaped(&b, p.key)
		b.WriteByte(keyEq)
		writeEscaped(&b, p.value)
	}
	b.WriteByte(keyClose)
	return b.String()
}

type labelPair struct{ key, value string }

// ParseSeriesKey decodes a key produced by SeriesKey back into its name and
// labels. It exists to make the encoding demonstrably invertible (see the
// round-trip fuzz target) and to let operational tooling read a raw bbolt key.
func ParseSeriesKey(key string) (string, map[string]string, error) {
	name, term, rest, err := scanToken(key, keyOpen)
	if err != nil {
		return "", nil, fmt.Errorf("series key %q: name: %w", key, err)
	}
	if term == 0 {
		return name, nil, nil // no label section
	}

	labels := make(map[string]string)
	for {
		var k, v string
		k, term, rest, err = scanToken(rest, keyEq)
		if err != nil {
			return "", nil, fmt.Errorf("series key %q: label name: %w", key, err)
		}
		if term != keyEq {
			return "", nil, fmt.Errorf("series key %q: label %q has no value", key, k)
		}
		v, term, rest, err = scanToken(rest, keySep, keyClose)
		if err != nil {
			return "", nil, fmt.Errorf("series key %q: label %q value: %w", key, k, err)
		}
		if _, dup := labels[k]; dup {
			return "", nil, fmt.Errorf("series key %q: duplicate label %q", key, k)
		}
		labels[k] = v

		switch term {
		case keySep:
			continue
		case keyClose:
			if rest != "" {
				return "", nil, fmt.Errorf("series key %q: trailing data after '}'", key)
			}
			return name, labels, nil
		default:
			return "", nil, fmt.Errorf("series key %q: unterminated label section", key)
		}
	}
}

// scanToken reads an escaped token up to the first unescaped byte in stops,
// which it returns as term (0 at end of input). Encountering an unescaped
// structural byte that is not a stop is an error: the encoder never emits one,
// so its presence means the key was not produced by SeriesKey.
func scanToken(s string, stops ...byte) (token string, term byte, rest string, err error) {
	// Fast path: the token contains no escape, so it is a substring of the input
	// and costs nothing to produce.
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isSpecial[c] {
			continue
		}
		if c == keyEsc {
			break // an escape: fall through to the copying path below
		}
		if slices.Contains(stops, c) {
			return s[:i], c, s[i+1:], nil
		}
		return "", 0, "", fmt.Errorf("unescaped %q", c)
	}
	if !strings.ContainsRune(s, keyEsc) {
		return s, 0, "", nil // clean token, ran to the end of input
	}

	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == keyEsc:
			if i+1 >= len(s) {
				return "", 0, "", fmt.Errorf("trailing escape")
			}
			i++
			if !isSpecial[s[i]] {
				return "", 0, "", fmt.Errorf("invalid escape %q", s[i-1:i+1])
			}
			b.WriteByte(s[i])
		case slices.Contains(stops, c):
			return b.String(), c, s[i+1:], nil
		case isSpecial[c]:
			return "", 0, "", fmt.Errorf("unescaped %q", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), 0, "", nil
}

// escape returns s with every structural byte backslash-escaped. The common case
// — no structural byte at all — returns s untouched, allocating nothing.
func escape(s string) string {
	n := escapedLen(s)
	if n == len(s) {
		return s
	}
	var b strings.Builder
	b.Grow(n)
	writeEscaped(&b, s)
	return b.String()
}

// writeEscaped copies s into b, backslash-prefixing structural bytes. It writes
// the runs between them with WriteString, so ordinary bytes move by memmove
// rather than one WriteByte call each.
func writeEscaped(b *strings.Builder, s string) {
	start := 0
	for i := 0; i < len(s); i++ {
		if !isSpecial[s[i]] {
			continue
		}
		b.WriteString(s[start:i])
		b.WriteByte(keyEsc)
		b.WriteByte(s[i])
		start = i + 1
	}
	b.WriteString(s[start:])
}

// escapedLen reports how long s becomes once escaped, so the builder can be
// sized exactly and never has to grow. It doubles as the "does this need
// escaping at all" test: escapedLen(s) == len(s) exactly when s is clean.
func escapedLen(s string) int {
	n := len(s)
	for i := 0; i < len(s); i++ {
		if isSpecial[s[i]] {
			n++
		}
	}
	return n
}
