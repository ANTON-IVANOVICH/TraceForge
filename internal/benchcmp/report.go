package benchcmp

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"text/tabwriter"
)

// Row is one benchmark/unit compared across two runs.
type Row struct {
	Key                  Key
	OldMedian, NewMedian float64
	OldSpread, NewSpread float64
	OldN, NewN           int
	Delta                float64 // percent change of the medians
	P                    float64
	Method               string
	Significant          bool
}

// Compare pairs up the benchmarks present in both collections. alpha is the
// significance threshold, conventionally 0.05: a p-value above it means the two
// samples are consistent with no change, and the delta is reported as "~".
//
// Reporting "~" for an insignificant 3% improvement is the entire value of this
// tool. Without it, every run of every benchmark shows a delta, and half of them
// point the wrong way.
func Compare(oldC, newC *Collection, alpha float64) (rows []Row, onlyOld, onlyNew []Key) {
	for _, key := range oldC.Keys() {
		oldS := oldC.Sample(key)
		newS := newC.Sample(key)
		if newS == nil {
			onlyOld = append(onlyOld, key)
			continue
		}

		p, method := MannWhitneyU(oldS.Values, newS.Values)
		oldMed, newMed := Median(oldS.Values), Median(newS.Values)

		rows = append(rows, Row{
			Key:         key,
			OldMedian:   oldMed,
			NewMedian:   newMed,
			OldSpread:   Spread(oldS.Values),
			NewSpread:   Spread(newS.Values),
			OldN:        len(oldS.Values),
			NewN:        len(newS.Values),
			Delta:       percentDelta(oldMed, newMed),
			P:           p,
			Method:      method,
			Significant: p < alpha,
		})
	}
	for _, key := range newC.Keys() {
		if oldC.Sample(key) == nil {
			onlyNew = append(onlyNew, key)
		}
	}
	return rows, onlyOld, onlyNew
}

func percentDelta(oldMed, newMed float64) float64 {
	if oldMed == 0 {
		if newMed == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return (newMed - oldMed) / oldMed * 100
}

// Render writes one table per unit. Units are kept apart because they are not
// comparable: an optimization that trades 5% more time for 80% fewer allocations
// is usually a win, and a single blended score would hide the trade.
//
// Every write goes through an errWriter, so a broken pipe (benchcmp | head) is
// reported once from Render rather than checked after each of a hundred Fprintfs.
func Render(w io.Writer, rows []Row, onlyOld, onlyNew []Key) error {
	byUnit := make(map[string][]Row)
	var units []string
	for _, r := range rows {
		if _, seen := byUnit[r.Key.Unit]; !seen {
			units = append(units, r.Key.Unit)
		}
		byUnit[r.Key.Unit] = append(byUnit[r.Key.Unit], r)
	}

	ew := &errWriter{w: w}
	for i, unit := range units {
		if i > 0 {
			_, _ = fmt.Fprintln(ew)
		}
		tw := tabwriter.NewWriter(ew, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintf(tw, "name\told %s\tnew %s\tdelta\t\n", unit, unit)
		for _, r := range byUnit[unit] {
			_, _ = fmt.Fprintf(tw, "%s\t%s ± %2.0f%%\t%s ± %2.0f%%\t%s\t\n",
				r.Key.Name,
				formatValue(r.OldMedian), r.OldSpread,
				formatValue(r.NewMedian), r.NewSpread,
				formatDelta(r))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(onlyOld) > 0 || len(onlyNew) > 0 {
		_, _ = fmt.Fprintln(ew)
	}
	// A benchmark that exists on one side only is not a regression, but silently
	// dropping it from the report is how a deleted benchmark goes unnoticed.
	for _, k := range dedupeNames(onlyOld) {
		_, _ = fmt.Fprintf(ew, "note: %s ran only in the old file\n", k)
	}
	for _, k := range dedupeNames(onlyNew) {
		_, _ = fmt.Fprintf(ew, "note: %s ran only in the new file\n", k)
	}
	return ew.err
}

// errWriter is Rob Pike's pattern: it remembers the first write error and turns
// every write after it into a no-op, so the caller checks once at the end.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return len(p), nil
	}
	n, err := e.w.Write(p)
	e.err = err
	return n, err
}

func dedupeNames(keys []Key) []string {
	seen := make(map[string]bool, len(keys))
	var out []string
	for _, k := range keys {
		if !seen[k.Name] {
			seen[k.Name] = true
			out = append(out, k.Name)
		}
	}
	slices.Sort(out)
	return out
}

func formatDelta(r Row) string {
	if !r.Significant {
		return fmt.Sprintf("~ (p=%.3f n=%d+%d)", r.P, r.OldN, r.NewN)
	}
	if math.IsInf(r.Delta, 0) {
		return fmt.Sprintf("? (p=%.3f n=%d+%d)", r.P, r.OldN, r.NewN)
	}
	return fmt.Sprintf("%+.2f%% (p=%.3f n=%d+%d)", r.Delta, r.P, r.OldN, r.NewN)
}

// formatValue keeps four significant digits, which is the precision `go test`
// itself prints and more than the noise justifies.
func formatValue(v float64) string {
	switch {
	case v == 0:
		return "0"
	case math.Abs(v) >= 100000:
		return fmt.Sprintf("%.4g", v)
	case math.Abs(v) >= 100:
		return fmt.Sprintf("%.1f", v)
	case math.Abs(v) >= 1:
		return fmt.Sprintf("%.3f", v)
	default:
		return strings.TrimRight(fmt.Sprintf("%.6f", v), "0")
	}
}
