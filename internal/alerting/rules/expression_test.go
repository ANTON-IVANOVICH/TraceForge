package rules

import (
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"metrics-system/internal/alerting/alert"
)

var evalAt = time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

// fakeQuerier serves canned series. Instant returns the last point of each
// series; Range returns all of them, so it mirrors StorageQuerier's contract
// without needing a store.
type fakeQuerier struct {
	series map[string][]Series // metric name -> series
	err    error
}

func (q *fakeQuerier) match(name string, matchers map[string]string) []Series {
	var out []Series
	for _, s := range q.series[name] {
		ok := true
		for k, v := range matchers {
			if s.Labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	return out
}

func (q *fakeQuerier) Instant(_ context.Context, name string, matchers map[string]string, _ time.Time) (Vector, error) {
	if q.err != nil {
		return nil, q.err
	}
	var out Vector
	for _, s := range q.match(name, matchers) {
		if len(s.Points) == 0 {
			continue
		}
		out = append(out, Sample{Labels: s.Labels, Value: s.Points[len(s.Points)-1].V})
	}
	sort.Slice(out, func(i, j int) bool {
		return alert.LabelsString(out[i].Labels) < alert.LabelsString(out[j].Labels)
	})
	return out, nil
}

func (q *fakeQuerier) Range(_ context.Context, name string, matchers map[string]string, _, _ time.Time) ([]Series, error) {
	if q.err != nil {
		return nil, q.err
	}
	return q.match(name, matchers), nil
}

func series(labels map[string]string, values ...float64) Series {
	pts := make([]Point, len(values))
	for i, v := range values {
		pts[i] = Point{T: evalAt.Add(time.Duration(i-len(values)+1) * time.Minute), V: v}
	}
	return Series{Labels: labels, Points: pts}
}

func lbl(kv ...string) map[string]string {
	m := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func mustEval(t *testing.T, q Querier, expr string) Vector {
	t.Helper()
	e, _, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	v, err := e.Eval(context.Background(), q, evalAt)
	if err != nil {
		t.Fatalf("Eval(%q): %v", expr, err)
	}
	return v
}

// render turns a vector into a stable, comparable string.
func render(v Vector) string {
	parts := make([]string, 0, len(v))
	for _, s := range v {
		parts = append(parts, alert.LabelsString(s.Labels)+"="+strconv.FormatFloat(s.Value, 'g', -1, 64))
	}
	sort.Strings(parts)
	return strings.Join(parts, " | ")
}

// TestFilterSemantics: `cpu > 90` yields the breaching samples with their own
// values, not booleans. That is what makes the vector an alert set.
func TestFilterSemantics(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{series: map[string][]Series{
		"cpu": {
			series(lbl("host", "a"), 95),
			series(lbl("host", "b"), 50),
			series(lbl("host", "c"), 99),
		},
	}}

	got := mustEval(t, q, "cpu > 90")
	if len(got) != 2 {
		t.Fatalf("got %d samples, want 2: %s", len(got), render(got))
	}
	for _, s := range got {
		if s.Value != 95 && s.Value != 99 {
			t.Fatalf("sample kept its comparison result instead of its value: %v", s)
		}
	}
}

func TestScalarOnTheLeft(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{series: map[string][]Series{
		"cpu": {series(lbl("host", "a"), 95), series(lbl("host", "b"), 10)},
	}}
	got := mustEval(t, q, "90 < cpu")
	if len(got) != 1 || got[0].Labels["host"] != "a" {
		t.Fatalf("got %s, want only host=a", render(got))
	}
}

func TestScalarComparison(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{}
	if got := mustEval(t, q, "2 > 1"); len(got) != 1 || got[0].Value != 2 {
		t.Fatalf("2 > 1 = %s, want a single sample carrying 2", render(got))
	}
	if got := mustEval(t, q, "1 > 2"); len(got) != 0 {
		t.Fatalf("1 > 2 = %s, want an empty vector", render(got))
	}
}

func TestVectorVectorComparisonMatchesOnLabels(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{series: map[string][]Series{
		"used":  {series(lbl("host", "a"), 90), series(lbl("host", "b"), 10)},
		"limit": {series(lbl("host", "a"), 80), series(lbl("host", "b"), 80)},
	}}
	got := mustEval(t, q, "used > limit")
	if len(got) != 1 || got[0].Labels["host"] != "a" || got[0].Value != 90 {
		t.Fatalf("got %s, want host=a with value 90", render(got))
	}
}

func TestArithmeticAndNaNDropping(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{series: map[string][]Series{
		"bytes": {series(lbl("host", "a"), 2048)},
	}}
	got := mustEval(t, q, "bytes / 1024")
	if len(got) != 1 || got[0].Value != 2 {
		t.Fatalf("got %s, want 2", render(got))
	}
	// 0/0 is NaN, which can never satisfy a later comparison, so it is dropped.
	if got := mustEval(t, q, "0 / 0"); len(got) != 0 {
		t.Fatalf("0/0 = %s, want an empty vector", render(got))
	}
}

func TestLogicalOperators(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{series: map[string][]Series{
		"cpu": {series(lbl("host", "a"), 95), series(lbl("host", "b"), 95)},
		"mem": {series(lbl("host", "a"), 99)},
	}}

	tests := map[string]string{
		"cpu > 90 and mem > 90":    `host="a"=95`,
		"cpu > 90 unless mem > 90": `host="b"=95`,
	}
	for expr, want := range tests {
		t.Run(expr, func(t *testing.T) {
			if got := render(mustEval(t, q, expr)); got != want {
				t.Fatalf("got %s, want %s", got, want)
			}
		})
	}

	if got := mustEval(t, q, "cpu > 90 or mem > 90"); len(got) != 2 {
		t.Fatalf("or produced %s, want both cpu samples (mem's label set matches a)", render(got))
	}
}

func TestLogicalOperatorRejectsScalar(t *testing.T) {
	t.Parallel()
	e, _, err := Parse("cpu and 1")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	q := &fakeQuerier{series: map[string][]Series{"cpu": {series(lbl("host", "a"), 1)}}}
	if _, err := e.Eval(context.Background(), q, evalAt); err == nil {
		t.Fatal("expected `and` with a scalar operand to fail")
	}
}

func TestAggregationByAndWithout(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{series: map[string][]Series{
		"disk": {
			series(lbl("host", "a", "mount", "/"), 10),
			series(lbl("host", "a", "mount", "/data"), 30),
			series(lbl("host", "b", "mount", "/"), 5),
		},
	}}

	if got := render(mustEval(t, q, "max by (host) (disk)")); got != `host="a"=30 | host="b"=5` {
		t.Fatalf("max by (host) = %s", got)
	}
	if got := render(mustEval(t, q, "sum without (mount) (disk)")); got != `host="a"=40 | host="b"=5` {
		t.Fatalf("sum without (mount) = %s", got)
	}
	// No grouping clause collapses everything into one unlabelled (scalar) sample.
	got := mustEval(t, q, "sum(disk)")
	if len(got) != 1 || got[0].Value != 45 || len(got[0].Labels) != 0 {
		t.Fatalf("sum(disk) = %s, want a single unlabelled 45", render(got))
	}
	if got := render(mustEval(t, q, "count by (host) (disk)")); got != `host="a"=2 | host="b"=1` {
		t.Fatalf("count by (host) = %s", got)
	}
}

// rate() must treat a decrease as a counter reset, not as negative traffic.
func TestRateHandlesCounterReset(t *testing.T) {
	t.Parallel()
	// 0,10,20 then a restart back to 5: increase is 20 + 5 = 25 over 3 minutes.
	q := &fakeQuerier{series: map[string][]Series{
		"requests": {series(lbl("host", "a"), 0, 10, 20, 5)},
	}}
	got := mustEval(t, q, "rate(requests[10m])")
	if len(got) != 1 {
		t.Fatalf("got %d samples, want 1", len(got))
	}
	want := 25.0 / (3 * 60) // three one-minute gaps
	if math.Abs(got[0].Value-want) > 1e-9 {
		t.Fatalf("rate = %v, want %v (a reset must not produce a negative rate)", got[0].Value, want)
	}
	if got[0].Value <= 0 {
		t.Fatal("rate went non-positive across a counter reset")
	}
}

func TestRangeFunctions(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{series: map[string][]Series{
		"cpu": {series(lbl("host", "a"), 10, 20, 60)},
	}}
	tests := map[string]float64{
		"avg_over_time(cpu[5m])":   30,
		"min_over_time(cpu[5m])":   10,
		"max_over_time(cpu[5m])":   60,
		"sum_over_time(cpu[5m])":   90,
		"count_over_time(cpu[5m])": 3,
		"last_over_time(cpu[5m])":  60,
		"delta(cpu[5m])":           50,
		"increase(cpu[5m])":        50,
	}
	for expr, want := range tests {
		t.Run(expr, func(t *testing.T) {
			got := mustEval(t, q, expr)
			if len(got) != 1 || math.Abs(got[0].Value-want) > 1e-9 {
				t.Fatalf("%s = %s, want %v", expr, render(got), want)
			}
		})
	}
}

// A series with a single point cannot yield a rate; it is skipped rather than
// reported as zero, which would read as "no traffic" instead of "unknown".
func TestRateSkipsSinglePointSeries(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{series: map[string][]Series{"requests": {series(lbl("host", "a"), 5)}}}
	if got := mustEval(t, q, "rate(requests[5m])"); len(got) != 0 {
		t.Fatalf("got %s, want an empty vector", render(got))
	}
}

func TestRangeSelectorOutsideRangeFunctionFails(t *testing.T) {
	t.Parallel()
	// The parser rejects it inside a call; here it appears bare, so Eval must.
	ref := &MetricRef{Name: "cpu", Range: 5 * time.Minute}
	if _, err := ref.Eval(context.Background(), &fakeQuerier{}, evalAt); err == nil {
		t.Fatal("a bare range selector must not evaluate")
	}
}

func TestInstantFunctions(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{series: map[string][]Series{"x": {series(lbl("host", "a"), -7.4)}}}
	tests := map[string]float64{
		"abs(x)":           7.4,
		"ceil(x)":          -7,
		"floor(x)":         -8,
		"round(x)":         -7,
		"clamp_min(x, 0)":  0,
		"clamp_max(x, -9)": -9,
	}
	for expr, want := range tests {
		t.Run(expr, func(t *testing.T) {
			got := mustEval(t, q, expr)
			if len(got) != 1 || math.Abs(got[0].Value-want) > 1e-9 {
				t.Fatalf("%s = %s, want %v", expr, render(got), want)
			}
		})
	}
}

func TestNonEqualityMatchersFilterInMemory(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{series: map[string][]Series{
		"up": {
			series(lbl("env", "prod", "host", "a"), 0),
			series(lbl("env", "staging", "host", "b"), 0),
			series(lbl("env", "production", "host", "c"), 0),
		},
	}}
	got := mustEval(t, q, `up{env=~"prod"} == 0`)
	if len(got) != 1 || got[0].Labels["host"] != "a" {
		t.Fatalf("got %s, want only the anchored prod match", render(got))
	}
	got = mustEval(t, q, `up{env!="prod"} == 0`)
	if len(got) != 2 {
		t.Fatalf("got %s, want the two non-prod series", render(got))
	}
}

func TestQuerierErrorPropagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("storage down")
	q := &fakeQuerier{err: sentinel}
	e, _, err := Parse("cpu > 1")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := e.Eval(context.Background(), q, evalAt); !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap %v", err, sentinel)
	}
}
