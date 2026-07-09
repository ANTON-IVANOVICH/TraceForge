package benchcmp

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"
)

const goBenchOutput = `goos: darwin
goarch: arm64
pkg: metrics-system/internal/server/storage
cpu: Apple M1
BenchmarkSeriesKey/labels=0-8         	513296517	         2.001 ns/op	       0 B/op	       0 allocs/op
BenchmarkSeriesKey/labels=0-8         	617430049	         1.946 ns/op	       0 B/op	       0 allocs/op
BenchmarkSeriesKey/labels=3-8         	  7593124	       158.0 ns/op	     216 B/op	       4 allocs/op
BenchmarkSeriesKey/labels=3-8         	  7401238	       159.2 ns/op	     216 B/op	       4 allocs/op
BenchmarkPipelineThroughput-8         	     1000	   1043210 ns/op	  95820 metrics/s
PASS
ok  	metrics-system/internal/server/storage	12.345s
`

func TestParse(t *testing.T) {
	c, err := Parse(strings.NewReader(goBenchOutput))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	nsKey := Key{Name: "SeriesKey/labels=3", Unit: "ns/op"}
	s := c.Sample(nsKey)
	if s == nil {
		t.Fatalf("missing sample %v; got keys %v", nsKey, c.Keys())
	}
	if len(s.Values) != 2 || s.Values[0] != 158.0 || s.Values[1] != 159.2 {
		t.Errorf("ns/op values: want [158 159.2], got %v", s.Values)
	}

	if got := c.Sample(Key{Name: "SeriesKey/labels=3", Unit: "allocs/op"}); got == nil || got.Values[0] != 4 {
		t.Errorf("allocs/op not parsed: %v", got)
	}
	// b.ReportMetric units must survive; they are how a benchmark says something
	// the framework has no name for.
	if got := c.Sample(Key{Name: "PipelineThroughput", Unit: "metrics/s"}); got == nil || got.Values[0] != 95820 {
		t.Errorf("custom unit not parsed: %v", got)
	}
	// The -8 suffix is GOMAXPROCS, not part of the benchmark's identity.
	if procs := c.GOMAXPROCS("SeriesKey/labels=0"); len(procs) != 1 || procs[0] != "8" {
		t.Errorf("GOMAXPROCS: want [8], got %v", procs)
	}
}

func TestParseRejectsOutputWithNoBenchmarks(t *testing.T) {
	if _, err := Parse(strings.NewReader("ok  \tfoo\t0.1s\n")); err == nil {
		t.Fatal("want an error for output containing no benchmark lines")
	}
}

func TestParseIgnoresLogLinesThatStartWithBenchmark(t *testing.T) {
	in := "Benchmark something went wrong\nBenchmarkX-8\t100\t5.0 ns/op\n"
	c, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(c.Keys()) != 1 {
		t.Errorf("want 1 key, got %v", c.Keys())
	}
}

func TestMedian(t *testing.T) {
	tests := []struct {
		name   string
		values []float64
		want   float64
	}{
		{"odd count", []float64{3, 1, 2}, 2},
		{"even count averages the middle pair", []float64{4, 1, 3, 2}, 2.5},
		{"single value", []float64{7}, 7},
		{"an outlier does not move it", []float64{1, 2, 3, 4, 1e9}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Median(tt.values); got != tt.want {
				t.Errorf("Median(%v) = %v, want %v", tt.values, got, tt.want)
			}
		})
	}
}

// The textbook case: two samples with no overlap at all. U = 0, and the exact
// two-sided p is 2 / C(10,5) = 2/252 = 0.0079365..., a number you can verify by
// hand and which pins the whole DP.
func TestMannWhitneyUExactCompleteSeparation(t *testing.T) {
	a := []float64{1, 2, 3, 4, 5}
	b := []float64{6, 7, 8, 9, 10}

	p, method := MannWhitneyU(a, b)
	if method != "exact" {
		t.Fatalf("want the exact test for untied samples of 5, got %q", method)
	}
	want := 2.0 / 252.0
	if math.Abs(p-want) > 1e-12 {
		t.Errorf("p = %.12f, want %.12f", p, want)
	}
}

// Symmetry: swapping the samples cannot change a two-sided p-value.
func TestMannWhitneyUIsSymmetric(t *testing.T) {
	a := []float64{12, 15, 11, 19, 14, 13, 16, 18}
	b := []float64{22, 25, 17, 29, 24, 23, 26, 28}

	p1, _ := MannWhitneyU(a, b)
	p2, _ := MannWhitneyU(b, a)
	if math.Abs(p1-p2) > 1e-12 {
		t.Errorf("p is not symmetric: %v vs %v", p1, p2)
	}
}

// Every observation identical: the variance of U is zero and there is nothing to
// reject. This is not academic — "0 allocs/op" ten times on each side is exactly
// this input, and a naive implementation divides by zero here.
func TestMannWhitneyUIdenticalSamplesRejectNothing(t *testing.T) {
	a := []float64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	b := []float64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}

	p, method := MannWhitneyU(a, b)
	if method != "normal" {
		t.Errorf("all-tied samples must fall back to the normal approximation, got %q", method)
	}
	if p != 1 {
		t.Errorf("p = %v, want 1 (no evidence of any difference)", p)
	}
}

func TestMannWhitneyUSameDistributionIsNotSignificant(t *testing.T) {
	a := []float64{100, 101, 102, 103, 104, 105}
	b := []float64{100.5, 101.5, 102.5, 103.5, 104.5, 99.5}

	p, _ := MannWhitneyU(a, b)
	if p < 0.05 {
		t.Errorf("two samples from the same distribution reported as different: p = %v", p)
	}
}

// A tied observation must not crash the exact path — it must switch to the
// approximation, because the exact distribution assumes distinct ranks.
//
// The expected p is computed by hand, and the whole normal branch hangs off it:
// pooled ranks 1,2,3,4,5.5,5.5,7,8,9,10 give R1 = 15.5 and U = 0.5; with the tie
// correction Σ(t³−t) = 6 the variance is (25/12)(11 − 6/90) = 22.7¯7 and
// z = (0.5 − 12.5 + 0.5)/4.7726 = −2.4096, so p = erfc(2.4096/√2) = 0.015971.
//
// Without a number here, every arithmetic mutant in the variance formula
// survives — which is precisely what `mutate ./internal/benchcmp` reported
// before this test existed.
func TestMannWhitneyUTiesUseTheNormalApproximation(t *testing.T) {
	a := []float64{1, 2, 3, 4, 5}
	b := []float64{5, 6, 7, 8, 9}

	p, method := MannWhitneyU(a, b)
	if method != "normal" {
		t.Errorf("a tie must disable the exact test, got %q", method)
	}
	if want := 0.0159707; math.Abs(p-want) > 1e-6 {
		t.Errorf("p = %.7f, want %.7f", p, want)
	}
}

// The tie correction shrinks the variance. Dropping it (Σ(t³−t) treated as 0)
// inflates sigma and makes the test say "not significant" when it is — the
// anticonservative direction, which is the dangerous one for a tool whose whole
// job is to stop you believing a difference that is not there.
func TestNormalPTieCorrectionShrinksTheVariance(t *testing.T) {
	const n1, n2 = 5, 5
	u := 0.5

	withTies := normalP(n1, n2, u, 6) // one tied pair
	withoutTies := normalP(n1, n2, u, 0)

	if !(withTies < withoutTies) {
		t.Errorf("the tie correction must reduce the variance and so the p-value: %v vs %v", withTies, withoutTies)
	}
}

// Two perfectly interleaved samples are the definition of "no difference".
func TestMannWhitneyUInterleavedSamples(t *testing.T) {
	a := []float64{2, 4, 6, 8, 10, 12}
	b := []float64{1, 3, 5, 7, 9, 11}

	p, method := MannWhitneyU(a, b)
	if method != "exact" {
		t.Fatalf("no ties here; want the exact test, got %q", method)
	}
	if want := 0.699134; math.Abs(p-want) > 1e-5 {
		t.Errorf("p = %.6f, want %.6f", p, want)
	}
}

// exactP's DP must reproduce a distribution whose median-ish points are known.
// P(U <= 0) for n1=n2=3 is 1/C(6,3) = 1/20, so the two-sided p is 0.1.
func TestExactPKnownSmallCase(t *testing.T) {
	if p := exactP(3, 3, 0); math.Abs(p-0.1) > 1e-12 {
		t.Errorf("exactP(3,3,0) = %v, want 0.1", p)
	}
	// U <= 1 covers two arrangements out of twenty.
	if p := exactP(3, 3, 1); math.Abs(p-0.2) > 1e-12 {
		t.Errorf("exactP(3,3,1) = %v, want 0.2", p)
	}
}

func TestMannWhitneyUEmptySample(t *testing.T) {
	p, method := MannWhitneyU(nil, []float64{1, 2})
	if p != 1 || method != "none" {
		t.Errorf("empty sample: got p=%v method=%q, want 1/none", p, method)
	}
}

// The exact distribution must sum to C(n1+n2, n1) arrangements: if the DP loses
// or double-counts a path, every p-value it produces is quietly wrong.
func TestExactDistributionCoversEveryArrangement(t *testing.T) {
	// p at the largest possible U is 1 (the whole distribution lies at or below).
	for n1 := 1; n1 <= 6; n1++ {
		for n2 := 1; n2 <= 6; n2++ {
			if p := exactP(n1, n2, float64(n1*n2)); math.Abs(p-1) > 1e-9 {
				t.Errorf("exactP(%d, %d, uMax) = %v, want the whole mass (capped at 1)", n1, n2, p)
			}
		}
	}
}

func TestRankPooledAveragesTies(t *testing.T) {
	ranks, tieTerm := rankPooled([]float64{1, 2}, []float64{2, 3})
	// Sorted: 1, 2, 2, 3 -> ranks 1, 2.5, 2.5, 4.
	want := []float64{1, 2.5, 2.5, 4}
	for i := range want {
		if ranks[i] != want[i] {
			t.Errorf("ranks = %v, want %v", ranks, want)
			break
		}
	}
	if tieTerm != 2*2*2-2 { // one group of 2: t³ - t = 6
		t.Errorf("tieTerm = %v, want 6", tieTerm)
	}
}

func TestSpread(t *testing.T) {
	if got := Spread([]float64{100}); got != 0 {
		t.Errorf("a single observation has no spread, got %v", got)
	}
	// median 100, half-range 10 -> 10%.
	if got := Spread([]float64{90, 100, 110}); math.Abs(got-10) > 1e-9 {
		t.Errorf("Spread = %v, want 10", got)
	}
}

func TestCompareMarksInsignificantDeltasWithTilde(t *testing.T) {
	oldC, _ := Parse(strings.NewReader(benchLines("X", []float64{100, 101, 99, 100, 102})))
	newC, _ := Parse(strings.NewReader(benchLines("X", []float64{100, 99, 101, 103, 98})))

	rows, _, _ := Compare(oldC, newC, 0.05)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Significant {
		t.Errorf("noise reported as a real change: p = %v", rows[0].P)
	}

	var buf bytes.Buffer
	if err := Render(&buf, rows, nil, nil); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(buf.String(), "~") {
		t.Errorf("an insignificant delta must render as ~:\n%s", buf.String())
	}
}

func TestCompareReportsARealImprovement(t *testing.T) {
	oldC, _ := Parse(strings.NewReader(benchLines("X", []float64{160, 158, 159, 161, 157, 160, 158, 159})))
	newC, _ := Parse(strings.NewReader(benchLines("X", []float64{128, 127, 129, 130, 126, 128, 129, 127})))

	rows, _, _ := Compare(oldC, newC, 0.05)
	if !rows[0].Significant {
		t.Fatalf("a clean 20%% improvement must be significant, p = %v", rows[0].P)
	}
	if rows[0].Delta > -15 {
		t.Errorf("delta = %.2f%%, want about -20%%", rows[0].Delta)
	}

	var buf bytes.Buffer
	if err := Render(&buf, rows, nil, nil); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := fmt.Sprintf("%+.2f%%", rows[0].Delta); !strings.Contains(buf.String(), want) {
		t.Errorf("report should carry the delta %s:\n%s", want, buf.String())
	}
}

// A benchmark that disappears between runs is not a regression, but it must not
// vanish from the report either.
func TestCompareReportsBenchmarksPresentOnOneSideOnly(t *testing.T) {
	oldC, _ := Parse(strings.NewReader(benchLines("Gone", []float64{1, 2})))
	newC, _ := Parse(strings.NewReader(benchLines("New", []float64{1, 2})))

	rows, onlyOld, onlyNew := Compare(oldC, newC, 0.05)
	if len(rows) != 0 {
		t.Errorf("no benchmark is shared, want 0 rows, got %d", len(rows))
	}
	if len(onlyOld) != 1 || onlyOld[0].Name != "Gone" {
		t.Errorf("onlyOld = %v", onlyOld)
	}
	if len(onlyNew) != 1 || onlyNew[0].Name != "New" {
		t.Errorf("onlyNew = %v", onlyNew)
	}

	var buf bytes.Buffer
	if err := Render(&buf, rows, onlyOld, onlyNew); err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{"Gone", "New"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("report omits %q:\n%s", want, buf.String())
		}
	}
}

func TestPercentDeltaHandlesAZeroBaseline(t *testing.T) {
	if got := percentDelta(0, 0); got != 0 {
		t.Errorf("0 -> 0 is no change, got %v", got)
	}
	if got := percentDelta(0, 5); !math.IsInf(got, 1) {
		t.Errorf("0 -> 5 has no finite percentage, got %v", got)
	}
}

func benchLines(name string, values []float64) string {
	var b strings.Builder
	for _, v := range values {
		b.WriteString("Benchmark" + name + "-8\t1000\t")
		b.WriteString(formatValue(v))
		b.WriteString(" ns/op\n")
	}
	return b.String()
}

func TestMixedGOMAXPROCS(t *testing.T) {
	// The same benchmark run at -8 and -4 is two machines wearing one name.
	in := "BenchmarkX-8\t100\t101 ns/op\nBenchmarkX-4\t100\t402 ns/op\nBenchmarkY-8\t100\t5 ns/op\n"
	c, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	mixed := c.MixedGOMAXPROCS()
	if len(mixed) != 1 || mixed[0] != "X" {
		t.Errorf("MixedGOMAXPROCS: want [X], got %v", mixed)
	}
	if procs := c.GOMAXPROCS("X"); len(procs) != 2 || procs[0] != "4" || procs[1] != "8" {
		t.Errorf("GOMAXPROCS(X): want sorted [4 8], got %v", procs)
	}
	if got := c.MixedGOMAXPROCS(); len(got) == 1 && c.GOMAXPROCS("Y") != nil && len(c.GOMAXPROCS("Y")) != 1 {
		t.Errorf("Y ran at one proc count; must not be flagged")
	}
}
