package promexport

import (
	"bytes"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// parsedSample is one decoded exposition line.
type parsedSample struct {
	name   string
	labels map[string]string
	value  float64
}

// parseExposition decodes the text exposition format back into samples. It is
// the inverse of Write, and its existence is the point of FuzzWriteRoundTrip: an
// encoding you can decode cannot collide, because a collision would give one
// input two decodings. HELP and TYPE lines are documentation, not series, so
// they are skipped.
func parseExposition(t *testing.T, s string) []parsedSample {
	t.Helper()
	var out []parsedSample
	for _, line := range strings.Split(s, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ps, err := parseSampleLine(line)
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		out = append(out, ps)
	}
	return out
}

func parseSampleLine(line string) (parsedSample, error) {
	ps := parsedSample{labels: map[string]string{}}

	// The name runs to the first '{' (labels) or ' ' (no labels). A metric name
	// contains neither, so this cannot split the name early.
	i := 0
	for i < len(line) && line[i] != '{' && line[i] != ' ' {
		i++
	}
	ps.name = line[:i]

	if i < len(line) && line[i] == '{' {
		i++ // past '{'
		for {
			j := i
			for j < len(line) && line[j] != '=' {
				j++
			}
			if j >= len(line) {
				return ps, fmt.Errorf("label without '='")
			}
			name := line[i:j]
			if j+1 >= len(line) || line[j+1] != '"' {
				return ps, fmt.Errorf("label value not quoted")
			}

			// The value ends at the first UNescaped quote. Because Write escapes
			// every quote, backslash and newline inside a value, the terminator is
			// unambiguous and the value decodes byte-for-byte.
			k := j + 2
			var val []byte
			for {
				if k >= len(line) {
					return ps, fmt.Errorf("unterminated label value")
				}
				c := line[k]
				if c == '\\' {
					if k+1 >= len(line) {
						return ps, fmt.Errorf("trailing escape")
					}
					switch line[k+1] {
					case '\\':
						val = append(val, '\\')
					case '"':
						val = append(val, '"')
					case 'n':
						val = append(val, '\n')
					default:
						return ps, fmt.Errorf("bad escape \\%c", line[k+1])
					}
					k += 2
					continue
				}
				if c == '"' {
					k++
					break
				}
				val = append(val, c)
				k++
			}
			if _, dup := ps.labels[name]; dup {
				return ps, fmt.Errorf("duplicate label %q", name)
			}
			ps.labels[name] = string(val)

			if k >= len(line) {
				return ps, fmt.Errorf("labels not closed")
			}
			switch line[k] {
			case ',':
				i = k + 1
				continue
			case '}':
				i = k + 1
			default:
				return ps, fmt.Errorf("unexpected %q after label value", line[k])
			}
			break
		}
		if i >= len(line) || line[i] != ' ' {
			return ps, fmt.Errorf("no single space before value")
		}
		i++
	} else if i < len(line) && line[i] == ' ' {
		i++
	}

	v, err := parseValue(line[i:])
	if err != nil {
		return ps, err
	}
	ps.value = v
	return ps, nil
}

func parseValue(s string) (float64, error) {
	switch s {
	case "+Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	case "NaN":
		return math.NaN(), nil
	}
	return strconv.ParseFloat(s, 64)
}

func sameFloat(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	return a == b
}

// FuzzWriteRoundTrip asserts the injectivity invariant: whatever bytes go into a
// name, label names, label values and value, once a family Validates and is
// written, parsing the output recovers every field exactly. An escaping that is
// not invertible is an escaping that collides, and this is what would catch it.
func FuzzWriteRoundTrip(f *testing.F) {
	// Seed with the bytes that carry structure in the format, plus unicode and
	// empties — the inputs most likely to break an escaping.
	f.Add("metric_name", "help text", "a", "\\", "b", "\"", 1.5)
	f.Add("x", "line1\nline2", "l1", "a\nb", "l2", "c\\d", math.Inf(1))
	f.Add("y", "", "k1", "", "k2", "cafe ☃ unicode", math.NaN())
	f.Add("z:col", "quote\"in help", "k1", "va,lue", "k2", "}brace{ =", math.Inf(-1))
	f.Add("m", "back\\slash", "a", "tab\tend", "b", `"quoted"`, math.Copysign(0, -1))

	f.Fuzz(func(t *testing.T, name, help, ln1, lv1, ln2, lv2 string, val float64) {
		fam := Family{
			Name: name,
			Help: help,
			Type: TypeGauge, // gauge so the value may be any float
			Samples: []Sample{{
				Labels: []Label{{ln1, lv1}, {ln2, lv2}},
				Value:  val,
			}},
		}
		if err := fam.Validate(); err != nil {
			t.Skip() // not a family the format can represent; nothing to round-trip
		}

		var buf bytes.Buffer
		if err := Write(&buf, []Family{fam}); err != nil {
			t.Fatalf("Write errored on a family that Validated: %v", err)
		}

		samples := parseExposition(t, buf.String())
		if len(samples) != 1 {
			t.Fatalf("got %d samples, want 1\noutput:\n%q", len(samples), buf.String())
		}
		got := samples[0]

		if got.name != name {
			t.Errorf("name: got %q want %q", got.name, name)
		}
		want := map[string]string{ln1: lv1, ln2: lv2}
		if !reflect.DeepEqual(got.labels, want) {
			t.Errorf("labels: got %v want %v\noutput:\n%q", got.labels, want, buf.String())
		}
		if !sameFloat(got.value, val) {
			t.Errorf("value: got %v want %v\noutput:\n%q", got.value, val, buf.String())
		}
	})
}
