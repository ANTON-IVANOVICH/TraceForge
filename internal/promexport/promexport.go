// Package promexport renders metrics in the Prometheus text exposition format
// (version 0.0.4) and serves them over HTTP, without pulling in
// prometheus/client_golang.
//
// The reason to write it from scratch is the reason internal/server/storage
// serieskey.go was written from scratch: the whole job is an encoding, and an
// encoding is only safe if it is unambiguous. client_golang would add a
// dependency tree to emit a format whose entire content is a few hundred lines
// of escaping rules — rules this project can state, test, and prove invertible
// itself. This module has already done exactly this for its series-key encoder,
// its WAL, its libpcap binding and its benchmark comparator; the exposition
// format is one more small, self-contained encoding, so it lives here.
//
// The escaping is the load-bearing part. A label value may contain a backslash,
// a double quote or a newline — the three bytes that also delimit the format —
// so each is escaped, and the escape is chosen so that decoding is
// deterministic. Read serieskey.go first: FuzzWriteRoundTrip in this package is
// the same proof that ParseSeriesKey is there. An escaping you cannot invert is
// an escaping that collides, and a collision silently merges two series.
package promexport

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Type is the exposition-format metric type of a family.
type Type uint8

// The three metric types this server exposes. Summaries and the OpenMetrics
// extensions are deliberately absent: nothing here produces them, and an
// unimplemented type in the switch is a clearer signal than a silent default.
const (
	// TypeCounter is a monotonically increasing value.
	TypeCounter Type = iota
	// TypeGauge is a value that may move in either direction.
	TypeGauge
	// TypeHistogram is a set of cumulative bucket counts plus a sum and a count.
	TypeHistogram
)

// String returns the exposition-format spelling of the type: "counter",
// "gauge" or "histogram".
func (t Type) String() string {
	switch t {
	case TypeCounter:
		return "counter"
	case TypeGauge:
		return "gauge"
	case TypeHistogram:
		return "histogram"
	default:
		return "unknown"
	}
}

// Label is one key/value pair attached to a sample.
type Label struct{ Name, Value string }

// Sample is one line of output. Suffix is "" for counters and gauges and one of
// "_bucket", "_sum" or "_count" for the members of a histogram family; it is
// appended to the family name to form the series name on the wire.
type Sample struct {
	Suffix string
	Labels []Label
	Value  float64
}

// Family is a group of samples that share a name, help string and type.
type Family struct {
	// Name is the metric name without any per-sample suffix. Counters end in
	// _total by convention, but that is the caller's choice, not enforced here.
	Name    string
	Help    string
	Type    Type
	Samples []Sample
}

// The suffixes that make up a histogram family. They are also the ordering
// rank: buckets, then the sum, then the count.
const (
	suffixBucket = "_bucket"
	suffixSum    = "_sum"
	suffixCount  = "_count"
)

// Gatherer is a source of metric families, collected once per scrape.
type Gatherer interface{ Gather() []Family }

// GathererFunc adapts a plain function to Gatherer.
type GathererFunc func() []Family

// Gather calls f.
func (f GathererFunc) Gather() []Family { return f() }

// ValidMetricName reports whether s matches [a-zA-Z_:][a-zA-Z0-9_:]*. The colon
// is allowed because Prometheus reserves it for recording-rule names, which are
// metric names.
func ValidMetricName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_', c == ':':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// ValidLabelName reports whether s matches [a-zA-Z_][a-zA-Z0-9_]*. The colon a
// metric name may carry is rejected here: colon is reserved for recording
// rules, which name series and not labels, so a label named "le:x" is a bug the
// caller wants caught rather than exposed. That single character is the whole
// difference between this and ValidMetricName.
func ValidLabelName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// Validate reports why the family cannot be exposed, or nil if it can. It is the
// gate that keeps a malformed family from corrupting a whole scrape: Write skips
// what fails here. The checks are the ones that make the output parseable and
// injective — a valid name, valid and unique label names per sample, a
// well-formed le on every histogram bucket, and no two samples that are the same
// series (which would decode ambiguously).
func (f Family) Validate() error {
	if !ValidMetricName(f.Name) {
		return fmt.Errorf("promexport: invalid metric name %q", f.Name)
	}
	switch f.Type {
	case TypeCounter, TypeGauge, TypeHistogram:
	default:
		return fmt.Errorf("promexport: metric %q has unknown type %d", f.Name, uint8(f.Type))
	}

	seen := make(map[string]struct{}, len(f.Samples))
	for _, s := range f.Samples {
		if err := validateSuffix(f.Type, s.Suffix); err != nil {
			return fmt.Errorf("promexport: metric %q: %w", f.Name, err)
		}

		names := make(map[string]struct{}, len(s.Labels))
		for _, l := range s.Labels {
			if !ValidLabelName(l.Name) {
				return fmt.Errorf("promexport: metric %q: invalid label name %q", f.Name, l.Name)
			}
			if _, dup := names[l.Name]; dup {
				return fmt.Errorf("promexport: metric %q: duplicate label %q", f.Name, l.Name)
			}
			names[l.Name] = struct{}{}
		}

		if f.Type == TypeHistogram && s.Suffix == suffixBucket {
			le, ok := findLabel(s.Labels, "le")
			if !ok {
				return fmt.Errorf("promexport: metric %q: bucket sample has no le label", f.Name)
			}
			if _, err := parseLe(le); err != nil {
				return fmt.Errorf("promexport: metric %q: %w", f.Name, err)
			}
		}

		// Two samples that render to the same series (same suffix, same label
		// set) would decode ambiguously and are rejected by promtool; catch them
		// here so one duplicated series does not corrupt the family.
		key := s.Suffix + "\x00" + canonicalLabelKey(s.Labels)
		if _, dup := seen[key]; dup {
			return fmt.Errorf("promexport: metric %q: duplicate series", f.Name)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// validateSuffix enforces that only histogram families carry per-sample
// suffixes, and only the three the format defines.
func validateSuffix(typ Type, suffix string) error {
	if typ == TypeHistogram {
		switch suffix {
		case suffixBucket, suffixSum, suffixCount:
			return nil
		}
		return fmt.Errorf("histogram sample has unexpected suffix %q", suffix)
	}
	if suffix != "" {
		return fmt.Errorf("%s sample has suffix %q; only histograms carry suffixes", typ, suffix)
	}
	return nil
}

// Write renders families in the text exposition format. A family that fails
// Validate is skipped and Write returns the first such error only after
// rendering the rest, so one bad metric never blanks an entire scrape. The
// output is built in a buffer and flushed in a single write, so a write error to
// w cannot leave a half-rendered family on the wire.
func Write(w io.Writer, families []Family) error {
	var buf bytes.Buffer
	var firstErr error
	keep := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	// A metric name may carry exactly one HELP and one TYPE line in the whole
	// document; a scraper rejects the second. Nothing in a single Family can
	// violate that, but a Handler stitching several Gatherers together can — and
	// the failure would be a scrape that parses on the day it ships and stops
	// parsing the day someone registers a gatherer twice.
	seen := make(map[string]struct{}, len(families))

	for i := range families {
		if err := families[i].Validate(); err != nil {
			keep(err)
			continue
		}
		if _, dup := seen[families[i].Name]; dup {
			keep(fmt.Errorf("promexport: metric %q gathered more than once", families[i].Name))
			continue
		}
		seen[families[i].Name] = struct{}{}
		renderFamily(&buf, families[i])
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return err
	}
	return firstErr
}

func renderFamily(buf *bytes.Buffer, f Family) {
	if f.Help != "" {
		buf.WriteString("# HELP ")
		buf.WriteString(f.Name)
		buf.WriteByte(' ')
		writeHelpEscaped(buf, f.Help)
		buf.WriteByte('\n')
	}
	buf.WriteString("# TYPE ")
	buf.WriteString(f.Name)
	buf.WriteByte(' ')
	buf.WriteString(f.Type.String())
	buf.WriteByte('\n')

	// Determinism is what makes the golden test possible: sort the samples into a
	// fixed order before rendering so the same family always produces the same
	// bytes.
	ord := make([]ordered, len(f.Samples))
	for i, s := range f.Samples {
		ord[i] = prepare(f.Type, s)
	}
	sort.SliceStable(ord, func(i, j int) bool { return lessOrdered(ord[i], ord[j]) < 0 })

	for i := range ord {
		o := &ord[i]
		buf.WriteString(f.Name)
		buf.WriteString(o.suffix)
		writeLabels(buf, o.labels)
		buf.WriteByte(' ')
		buf.WriteString(formatValue(o.value))
		buf.WriteByte('\n')
	}
}

// ordered is a sample prepared for rendering: its labels are put into the
// canonical order (sorted by name, with le last for histograms) and the fields
// the comparator needs are precomputed.
type ordered struct {
	labels []Label
	suffix string
	value  float64
	group  []Label // labels excluding le, so samples of one series group together
	rank   int     // bucket < sum < count
	le     float64 // numeric le of a bucket, for ascending order with +Inf last
}

func prepare(typ Type, s Sample) ordered {
	ls := make([]Label, len(s.Labels))
	copy(ls, s.Labels)
	sort.SliceStable(ls, func(i, j int) bool { return ls[i].Name < ls[j].Name })

	o := ordered{suffix: s.Suffix, value: s.Value, rank: suffixRank(s.Suffix)}
	if typ != TypeHistogram {
		o.labels = ls
		o.group = ls
		return o
	}

	// The le label must render last, and buckets sort by it numerically rather
	// than lexically ("2" must precede "10", and "+Inf" must come last), so it is
	// held apart from the labels that identify the series.
	nonLe := make([]Label, 0, len(ls))
	var le *Label
	for i := range ls {
		if ls[i].Name == "le" {
			le = &ls[i]
			continue
		}
		nonLe = append(nonLe, ls[i])
	}
	o.group = nonLe
	if le == nil {
		o.labels = nonLe
		return o
	}
	o.le, _ = parseLe(le.Value) // Validate already accepted every bucket's le
	withLe := make([]Label, len(nonLe), len(nonLe)+1)
	copy(withLe, nonLe)
	o.labels = append(withLe, *le)
	return o
}

// lessOrdered orders two prepared samples: first by the labels that identify the
// series, then by suffix rank so a series' buckets precede its sum and count,
// then by le so buckets ascend with +Inf last.
func lessOrdered(a, b ordered) int {
	n := len(a.group)
	if len(b.group) < n {
		n = len(b.group)
	}
	for i := 0; i < n; i++ {
		if c := strings.Compare(a.group[i].Name, b.group[i].Name); c != 0 {
			return c
		}
		if c := strings.Compare(a.group[i].Value, b.group[i].Value); c != 0 {
			return c
		}
	}
	if len(a.group) != len(b.group) {
		return len(a.group) - len(b.group)
	}
	if a.rank != b.rank {
		return a.rank - b.rank
	}
	switch {
	case a.le < b.le:
		return -1
	case a.le > b.le:
		return 1
	default:
		return 0
	}
}

func suffixRank(s string) int {
	switch s {
	case suffixSum:
		return 1
	case suffixCount:
		return 2
	default:
		return 0 // a bare counter/gauge line, or a bucket
	}
}

func writeLabels(buf *bytes.Buffer, labels []Label) {
	if len(labels) == 0 {
		return
	}
	buf.WriteByte('{')
	for i, l := range labels {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(l.Name)
		buf.WriteString(`="`)
		writeValueEscaped(buf, l.Value)
		buf.WriteByte('"')
	}
	buf.WriteByte('}')
}

// writeHelpEscaped escapes a HELP string: backslash and newline only. A double
// quote is intentionally left alone — HELP is not quote-delimited, so escaping a
// quote there would put a stray backslash into the documentation.
func writeHelpEscaped(buf *bytes.Buffer, s string) {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		default:
			buf.WriteByte(s[i])
		}
	}
}

// writeValueEscaped escapes a label value: backslash, double quote and newline.
// These are exactly the bytes that would otherwise end the value early or spill
// onto the next line, which is what makes the encoding invertible.
func writeValueEscaped(buf *bytes.Buffer, s string) {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			buf.WriteString(`\\`)
		case '"':
			buf.WriteString(`\"`)
		case '\n':
			buf.WriteString(`\n`)
		default:
			buf.WriteByte(s[i])
		}
	}
}

// formatValue renders a float as the format wants it. The infinities and NaN
// have their own spellings because FormatFloat would otherwise emit "+Inf" as
// "+Inf" on some paths but the format mandates these exact three tokens.
func formatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	default:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
}

// parseLe converts a bucket's le label back to a number so buckets can be
// ordered. It rejects anything that is not a real bound, including NaN, because
// a bucket boundary that cannot be ordered cannot be placed.
func parseLe(s string) (float64, error) {
	if s == "+Inf" {
		return math.Inf(1), nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("bucket le %q is not a number", s)
	}
	if math.IsNaN(v) {
		return 0, fmt.Errorf("bucket le %q is NaN", s)
	}
	return v, nil
}

func findLabel(labels []Label, name string) (string, bool) {
	for _, l := range labels {
		if l.Name == name {
			return l.Value, true
		}
	}
	return "", false
}

// canonicalLabelKey builds an order-independent, injective key for a label set,
// used to detect two samples that are secretly the same series. It is the same
// lesson as serieskey.go: an unescaped join collides the moment a value contains
// the separator, so the separator is escaped and so is the escape.
func canonicalLabelKey(labels []Label) string {
	ls := make([]Label, len(labels))
	copy(ls, labels)
	sort.SliceStable(ls, func(i, j int) bool { return ls[i].Name < ls[j].Name })
	var buf []byte
	for _, l := range ls {
		buf = appendField(buf, l.Name)
		buf = appendField(buf, l.Value)
	}
	return string(buf)
}

// The separator and escape used by the injective field joins (canonicalLabelKey
// and the Vec map key). The unit separator almost never occurs in a label, so
// the escape is almost always a no-op, but it is applied unconditionally because
// "almost never" is exactly the collision a metrics endpoint must not have.
const (
	fieldSep = '\x1f'
	fieldEsc = '\\'
)

func appendField(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		if s[i] == fieldSep || s[i] == fieldEsc {
			dst = append(dst, fieldEsc)
		}
		dst = append(dst, s[i])
	}
	return append(dst, fieldSep)
}
