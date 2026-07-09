package rules

import (
	"context"
	"regexp"
	"strconv"
	"testing"
)

// Package-level sinks defeat dead-code elimination: without a visible use the
// compiler is free to drop the benchmarked call entirely.
var (
	sinkExpr Expression
	sinkVec  Vector
	sinkStr  string
	sinkBool bool
	sinkErr  error
)

// benchCases spans the complexity axis the parser and printer actually walk: a
// bare comparison, a labelled selector, a range function, an aggregation, and a
// deep boolean chain that stresses the precedence-climbing productions.
var benchCases = []struct {
	name, expr string
}{
	{"simple", "cpu > 90"},
	{"labelled", `cpu{host="web-1", env=~"prod.*"} > 90`},
	{"function", "rate(http_requests_total[5m]) > 1000"},
	{"aggregation", "avg by (host) (cpu_usage_percent) > 75"},
	{"deep_boolean", "a > 1 and b > 2 or c > 3 unless d > 4 and e > 5 or f > 6 and g > 7"},
}

func BenchmarkParse(b *testing.B) {
	for _, tc := range benchCases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sinkExpr, _, sinkErr = Parse(tc.expr)
			}
		})
	}
}

func BenchmarkExpressionString(b *testing.B) {
	for _, tc := range benchCases {
		expr, _, err := Parse(tc.expr)
		if err != nil {
			b.Fatalf("parse %q: %v", tc.expr, err)
		}
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sinkStr = expr.String()
			}
		})
	}
}

// benchQuerier serves nSeries series of nPoints points each, one per host, so a
// range function has real data to reduce and an aggregation has real groups to
// form. Values climb monotonically, which keeps rate() positive without tripping
// the counter-reset path.
func benchQuerier(nSeries, nPoints int) *fakeQuerier {
	ss := make([]Series, nSeries)
	for i := range ss {
		vals := make([]float64, nPoints)
		for j := range vals {
			vals[j] = float64(i*nPoints + j)
		}
		ss[i] = series(lbl("host", "web-"+strconv.Itoa(i)), vals...)
	}
	return &fakeQuerier{series: map[string][]Series{"cpu": ss}}
}

func BenchmarkEvaluate(b *testing.B) {
	exprs := []struct {
		name, expr string
	}{
		{"rate", "rate(cpu[5m]) > 90"},
		{"agg_by_host", "avg(cpu) by (host) > 75"},
	}
	for _, ex := range exprs {
		expr, _, err := Parse(ex.expr)
		if err != nil {
			b.Fatalf("parse %q: %v", ex.expr, err)
		}
		for _, n := range []int{1, 10, 100} {
			q := benchQuerier(n, 30)
			b.Run(ex.name+"/series="+strconv.Itoa(n), func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					sinkVec, sinkErr = expr.Eval(context.Background(), q, evalAt)
				}
			})
		}
	}
}

// BenchmarkLabelMatcher isolates the per-series matcher cost, the inner loop of
// every instant query: equality is a string compare, regex pays the RE2 engine.
func BenchmarkLabelMatcher(b *testing.B) {
	eq := LabelMatcher{Name: "host", Op: MatchEq, Value: "web-1"}
	re := LabelMatcher{Name: "host", Op: MatchRe, Value: "web-.*"}
	re.re = regexp.MustCompile("^(?:web-.*)$")

	b.Run("equality", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkBool = eq.matches("web-1")
		}
	})
	b.Run("regex", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkBool = re.matches("web-1")
		}
	})
}
