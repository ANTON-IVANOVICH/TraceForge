package rules

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"metrics-system/internal/alerting/alert"
)

// This file defines the AST the parser builds and the semantics of evaluating
// it against a Querier. The evaluation model mirrors PromQL: expressions
// evaluate to an instant Vector (zero or more labelled samples), a Vector of a
// single unlabelled sample is treated as a scalar, and comparison against a
// scalar filters rather than rewriting values so that `cpu > 90` reduces to
// "the samples that are alerting".

// aggregationOps are the reducer keywords recognised in operator position; the
// parser treats them as reserved so a series literally named "sum" is not
// allowed (it would be indistinguishable from an aggregation otherwise).
var aggregationOps = map[string]bool{
	"sum": true, "avg": true, "min": true, "max": true, "count": true, "stddev": true,
}

// rangeFunctions consume a range-vector selector (metric[5m]); their argument
// must be a MetricRef with a non-zero Range.
var rangeFunctions = map[string]bool{
	"rate": true, "increase": true, "delta": true,
	"avg_over_time": true, "min_over_time": true, "max_over_time": true,
	"sum_over_time": true, "count_over_time": true, "last_over_time": true,
	"stddev_over_time": true,
}

// instantFunctions map to their fixed arity; they operate on instant vectors.
var instantFunctions = map[string]int{
	"abs": 1, "ceil": 1, "floor": 1, "round": 1,
	"clamp_min": 2, "clamp_max": 2,
}

// isFunction reports whether name is a callable (used by the parser to reject
// unknown functions at parse time with a position).
func isFunction(name string) bool {
	if rangeFunctions[name] {
		return true
	}
	_, ok := instantFunctions[name]
	return ok
}

// isScalar reports whether v behaves as a scalar: exactly one sample carrying no
// labels. NumberLiteral and a clauseless aggregation both produce this shape.
func isScalar(v Vector) bool {
	return len(v) == 1 && len(v[0].Labels) == 0
}

// scalar returns the value of a scalar vector; callers must guard with isScalar.
func scalar(v Vector) float64 { return v[0].Value }

// ---------------------------------------------------------------------------
// NumberLiteral
// ---------------------------------------------------------------------------

// NumberLiteral is a constant scalar such as 90 or 1.5e3.
type NumberLiteral struct {
	Value float64
}

// Eval returns the constant as a single unlabelled sample (a scalar).
func (n *NumberLiteral) Eval(_ context.Context, _ Querier, _ time.Time) (Vector, error) {
	return Vector{{Labels: nil, Value: n.Value}}, nil
}

func (n *NumberLiteral) String() string {
	return strconv.FormatFloat(n.Value, 'g', -1, 64)
}

// ---------------------------------------------------------------------------
// MetricRef
// ---------------------------------------------------------------------------

// MatchOp is the operator of a label matcher.
type MatchOp string

const (
	MatchEq  MatchOp = "="
	MatchNeq MatchOp = "!="
	MatchRe  MatchOp = "=~"
	MatchNre MatchOp = "!~"
)

// LabelMatcher selects series by one label. Equality matchers are pushed down
// to storage; the others are applied in memory because the store can only index
// exact values.
type LabelMatcher struct {
	Name  string
	Op    MatchOp
	Value string

	// re is the compiled, fully-anchored pattern for =~ and !~. It is compiled
	// once at parse time so evaluation never pays for regexp.Compile.
	re *regexp.Regexp
}

func (m LabelMatcher) String() string {
	return m.Name + string(m.Op) + quoteString(m.Value)
}

// matches reports whether the matcher holds for a series' value of its label.
func (m LabelMatcher) matches(val string) bool {
	switch m.Op {
	case MatchEq:
		return val == m.Value
	case MatchNeq:
		return val != m.Value
	case MatchRe:
		return m.re.MatchString(val)
	case MatchNre:
		return !m.re.MatchString(val)
	}
	return false
}

// MetricRef selects a series by name and matchers. With a zero Range it is an
// instant selector; with a non-zero Range it is a range selector that only
// range functions (rate, avg_over_time, …) may consume.
type MetricRef struct {
	Name     string
	Matchers []LabelMatcher
	Range    time.Duration
}

// splitMatchers separates the equality matchers (pushed down to the store) from
// the rest (applied in memory).
func (m *MetricRef) splitMatchers() (map[string]string, []LabelMatcher) {
	eq := make(map[string]string)
	var rest []LabelMatcher
	for _, mm := range m.Matchers {
		if mm.Op == MatchEq {
			eq[mm.Name] = mm.Value
		} else {
			rest = append(rest, mm)
		}
	}
	return eq, rest
}

// keep reports whether labels satisfy every in-memory matcher.
func keep(labels map[string]string, matchers []LabelMatcher) bool {
	for _, mm := range matchers {
		if !mm.matches(labels[mm.Name]) {
			return false
		}
	}
	return true
}

// Eval selects the latest sample of each matching series. A range selector has
// no instant value on its own, so using one outside a range function is an
// error rather than a silent empty result.
func (m *MetricRef) Eval(ctx context.Context, q Querier, at time.Time) (Vector, error) {
	if m.Range != 0 {
		return nil, fmt.Errorf("range vector selector %s must be used inside a range function such as rate()", m.String())
	}
	eq, rest := m.splitMatchers()
	vec, err := q.Instant(ctx, m.Name, eq, at)
	if err != nil {
		return nil, fmt.Errorf("instant query %s: %w", m.Name, err)
	}
	out := make(Vector, 0, len(vec))
	for _, s := range vec {
		if keep(s.Labels, rest) {
			out = append(out, s)
		}
	}
	return out, nil
}

// EvalRange returns the raw points of each matching series over [at-Range, at],
// the input range functions reduce. It exists separately from Eval because a
// range selector is not a valid instant expression.
func (m *MetricRef) EvalRange(ctx context.Context, q Querier, at time.Time) ([]Series, error) {
	if m.Range == 0 {
		return nil, fmt.Errorf("%s is not a range vector selector (missing [duration])", m.String())
	}
	eq, rest := m.splitMatchers()
	series, err := q.Range(ctx, m.Name, eq, at.Add(-m.Range), at)
	if err != nil {
		return nil, fmt.Errorf("range query %s: %w", m.Name, err)
	}
	out := make([]Series, 0, len(series))
	for _, s := range series {
		if keep(s.Labels, rest) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *MetricRef) String() string {
	var b strings.Builder
	b.WriteString(m.Name)
	if len(m.Matchers) > 0 {
		b.WriteByte('{')
		for i, mm := range m.Matchers {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(mm.String())
		}
		b.WriteByte('}')
	}
	if m.Range != 0 {
		b.WriteByte('[')
		b.WriteString(m.Range.String())
		b.WriteByte(']')
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// UnaryOp
// ---------------------------------------------------------------------------

// UnaryOp is a prefix operator; only negation is defined.
type UnaryOp struct {
	Op      string
	Operand Expression
}

// Eval negates every value in the operand vector (scalars included).
func (u *UnaryOp) Eval(ctx context.Context, q Querier, at time.Time) (Vector, error) {
	v, err := u.Operand.Eval(ctx, q, at)
	if err != nil {
		return nil, err
	}
	out := make(Vector, len(v))
	for i, s := range v {
		out[i] = Sample{Labels: s.Labels, Value: -s.Value}
	}
	return out, nil
}

func (u *UnaryOp) String() string { return u.Op + u.Operand.String() }

// ---------------------------------------------------------------------------
// BinaryOp
// ---------------------------------------------------------------------------

// BinaryOp is an arithmetic, comparison or logical operator. String always
// parenthesises so precedence never has to be re-derived on a round-trip.
type BinaryOp struct {
	Op          string
	Left, Right Expression
}

// Eval dispatches on the operator class. Both operands are always evaluated —
// the language has no short-circuiting because logical operators are set
// operations on vectors, not boolean tests.
func (b *BinaryOp) Eval(ctx context.Context, q Querier, at time.Time) (Vector, error) {
	left, err := b.Left.Eval(ctx, q, at)
	if err != nil {
		return nil, err
	}
	right, err := b.Right.Eval(ctx, q, at)
	if err != nil {
		return nil, err
	}
	switch b.Op {
	case "and", "or", "unless":
		return evalLogical(b.Op, left, right)
	case "==", "!=", ">", "<", ">=", "<=":
		return evalComparison(b.Op, left, right), nil
	default:
		return evalArithmetic(b.Op, left, right), nil
	}
}

func (b *BinaryOp) String() string {
	return "(" + b.Left.String() + " " + b.Op + " " + b.Right.String() + ")"
}

// indexByLabels maps each sample's label set to its value for vector-vector
// joins. On duplicate label sets the last sample wins, matching how a store
// would surface a single series per label set.
func indexByLabels(v Vector) map[string]float64 {
	idx := make(map[string]float64, len(v))
	for _, s := range v {
		idx[alert.LabelsString(s.Labels)] = s.Value
	}
	return idx
}

func labelSet(v Vector) map[string]bool {
	set := make(map[string]bool, len(v))
	for _, s := range v {
		set[alert.LabelsString(s.Labels)] = true
	}
	return set
}

// evalComparison implements filter semantics: it keeps the samples for which the
// comparison holds and preserves their original value, so the result is the set
// of series that breach the threshold.
func evalComparison(op string, left, right Vector) Vector {
	ls, rs := isScalar(left), isScalar(right)
	out := Vector{}
	switch {
	case ls && rs:
		if compare(op, scalar(left), scalar(right)) {
			return Vector{{Labels: nil, Value: scalar(left)}}
		}
		return out
	case rs:
		s := scalar(right)
		for _, smp := range left {
			if compare(op, smp.Value, s) {
				out = append(out, smp)
			}
		}
	case ls:
		s := scalar(left)
		for _, smp := range right {
			if compare(op, s, smp.Value) {
				out = append(out, smp)
			}
		}
	default:
		idx := indexByLabels(right)
		for _, smp := range left {
			rv, ok := idx[alert.LabelsString(smp.Labels)]
			if ok && compare(op, smp.Value, rv) {
				out = append(out, smp)
			}
		}
	}
	return out
}

// evalArithmetic transforms values, keeping labels. Results that are NaN are
// dropped: a NaN can never satisfy a later comparison, so carrying it forward
// would only produce phantom series.
func evalArithmetic(op string, left, right Vector) Vector {
	ls, rs := isScalar(left), isScalar(right)
	out := Vector{}
	switch {
	case ls && rs:
		r := applyArith(op, scalar(left), scalar(right))
		if math.IsNaN(r) {
			return out
		}
		return Vector{{Labels: nil, Value: r}}
	case rs:
		s := scalar(right)
		for _, smp := range left {
			r := applyArith(op, smp.Value, s)
			if math.IsNaN(r) {
				continue
			}
			out = append(out, Sample{Labels: smp.Labels, Value: r})
		}
	case ls:
		s := scalar(left)
		for _, smp := range right {
			r := applyArith(op, s, smp.Value)
			if math.IsNaN(r) {
				continue
			}
			out = append(out, Sample{Labels: smp.Labels, Value: r})
		}
	default:
		idx := indexByLabels(right)
		for _, smp := range left {
			rv, ok := idx[alert.LabelsString(smp.Labels)]
			if !ok {
				continue
			}
			r := applyArith(op, smp.Value, rv)
			if math.IsNaN(r) {
				continue
			}
			out = append(out, Sample{Labels: smp.Labels, Value: r})
		}
	}
	return out
}

// evalLogical implements set operations on label sets. It rejects scalar
// operands: `and`/`or`/`unless` join series, and a scalar has no series to join.
func evalLogical(op string, left, right Vector) (Vector, error) {
	if isScalar(left) || isScalar(right) {
		return nil, fmt.Errorf("logical operator %q requires vector operands, not a scalar", op)
	}
	out := Vector{}
	switch op {
	case "and":
		set := labelSet(right)
		for _, smp := range left {
			if set[alert.LabelsString(smp.Labels)] {
				out = append(out, smp)
			}
		}
	case "unless":
		set := labelSet(right)
		for _, smp := range left {
			if !set[alert.LabelsString(smp.Labels)] {
				out = append(out, smp)
			}
		}
	case "or":
		leftSet := labelSet(left)
		out = append(out, left...)
		for _, smp := range right {
			if !leftSet[alert.LabelsString(smp.Labels)] {
				out = append(out, smp)
			}
		}
	}
	return out, nil
}

func compare(op string, a, b float64) bool {
	switch op {
	case "==":
		return a == b
	case "!=":
		return a != b
	case ">":
		return a > b
	case "<":
		return a < b
	case ">=":
		return a >= b
	case "<=":
		return a <= b
	}
	return false
}

func applyArith(op string, a, b float64) float64 {
	switch op {
	case "+":
		return a + b
	case "-":
		return a - b
	case "*":
		return a * b
	case "/":
		return a / b
	case "%":
		return math.Mod(a, b)
	}
	return math.NaN()
}

// ---------------------------------------------------------------------------
// FunctionCall
// ---------------------------------------------------------------------------

// FunctionCall is either a range function (rate, avg_over_time, …) or an instant
// function (abs, clamp_min, …).
type FunctionCall struct {
	Name string
	Args []Expression
}

func (f *FunctionCall) Eval(ctx context.Context, q Querier, at time.Time) (Vector, error) {
	switch {
	case rangeFunctions[f.Name]:
		return f.evalRange(ctx, q, at)
	case instantFunctions[f.Name] != 0:
		return f.evalInstant(ctx, q, at)
	default:
		return nil, fmt.Errorf("unknown function %q", f.Name)
	}
}

// evalRange reduces each series in the argument's range to one sample. The
// argument must be a range-vector selector; anything else (a scalar, an instant
// selector, an arithmetic expression) has no points to reduce.
func (f *FunctionCall) evalRange(ctx context.Context, q Querier, at time.Time) (Vector, error) {
	if len(f.Args) != 1 {
		return nil, fmt.Errorf("%s expects exactly one argument, got %d", f.Name, len(f.Args))
	}
	mr, ok := f.Args[0].(*MetricRef)
	if !ok || mr.Range == 0 {
		return nil, fmt.Errorf("%s requires a range vector selector like metric[5m]", f.Name)
	}
	series, err := mr.EvalRange(ctx, q, at)
	if err != nil {
		return nil, err
	}
	out := Vector{}
	for _, s := range series {
		if v, ok := reduceRange(f.Name, s.Points); ok {
			out = append(out, Sample{Labels: alert.CloneLabels(s.Labels), Value: v})
		}
	}
	return out, nil
}

func (f *FunctionCall) evalInstant(ctx context.Context, q Querier, at time.Time) (Vector, error) {
	arity := instantFunctions[f.Name]
	if len(f.Args) != arity {
		return nil, fmt.Errorf("%s expects %d argument(s), got %d", f.Name, arity, len(f.Args))
	}
	vec, err := f.Args[0].Eval(ctx, q, at)
	if err != nil {
		return nil, err
	}
	switch f.Name {
	case "abs", "ceil", "floor", "round":
		out := make(Vector, 0, len(vec))
		for _, s := range vec {
			out = append(out, Sample{Labels: s.Labels, Value: applyMath(f.Name, s.Value)})
		}
		return out, nil
	default: // clamp_min, clamp_max
		bound, err := f.Args[1].Eval(ctx, q, at)
		if err != nil {
			return nil, err
		}
		if !isScalar(bound) {
			return nil, fmt.Errorf("%s: second argument must be a scalar", f.Name)
		}
		s := scalar(bound)
		out := make(Vector, 0, len(vec))
		for _, smp := range vec {
			v := smp.Value
			if f.Name == "clamp_min" {
				v = math.Max(v, s)
			} else {
				v = math.Min(v, s)
			}
			out = append(out, Sample{Labels: smp.Labels, Value: v})
		}
		return out, nil
	}
}

func applyMath(name string, v float64) float64 {
	switch name {
	case "abs":
		return math.Abs(v)
	case "ceil":
		return math.Ceil(v)
	case "floor":
		return math.Floor(v)
	case "round":
		return math.Round(v)
	}
	return v
}

func (f *FunctionCall) String() string {
	parts := make([]string, len(f.Args))
	for i, a := range f.Args {
		parts[i] = a.String()
	}
	return f.Name + "(" + strings.Join(parts, ", ") + ")"
}

// reduceRange collapses a series' points into the single value the range
// function produces. It returns ok=false to skip a series that cannot be
// reduced (e.g. a rate over fewer than two points), so callers drop it rather
// than emit a misleading zero.
func reduceRange(name string, pts []Point) (float64, bool) {
	switch name {
	case "rate":
		if len(pts) < 2 {
			return 0, false
		}
		span := pts[len(pts)-1].T.Sub(pts[0].T).Seconds()
		if span <= 0 {
			return 0, false
		}
		return counterIncrease(pts) / span, true
	case "increase":
		if len(pts) < 2 {
			return 0, false
		}
		return counterIncrease(pts), true
	case "delta":
		if len(pts) < 2 {
			return 0, false
		}
		return pts[len(pts)-1].V - pts[0].V, true
	case "sum_over_time":
		if len(pts) == 0 {
			return 0, false
		}
		var s float64
		for _, p := range pts {
			s += p.V
		}
		return s, true
	case "avg_over_time":
		if len(pts) == 0 {
			return 0, false
		}
		var s float64
		for _, p := range pts {
			s += p.V
		}
		return s / float64(len(pts)), true
	case "min_over_time":
		if len(pts) == 0 {
			return 0, false
		}
		m := pts[0].V
		for _, p := range pts {
			if p.V < m {
				m = p.V
			}
		}
		return m, true
	case "max_over_time":
		if len(pts) == 0 {
			return 0, false
		}
		m := pts[0].V
		for _, p := range pts {
			if p.V > m {
				m = p.V
			}
		}
		return m, true
	case "count_over_time":
		if len(pts) == 0 {
			return 0, false
		}
		return float64(len(pts)), true
	case "last_over_time":
		if len(pts) == 0 {
			return 0, false
		}
		return pts[len(pts)-1].V, true
	case "stddev_over_time":
		if len(pts) == 0 {
			return 0, false
		}
		vals := make([]float64, len(pts))
		for i, p := range pts {
			vals[i] = p.V
		}
		return populationStddev(vals), true
	}
	return 0, false
}

// counterIncrease sums the positive deltas between consecutive points, treating
// any decrease as a counter reset (the counter fell to zero and climbed back to
// the new value). Without this, a service restart would register as a large
// negative rate.
func counterIncrease(pts []Point) float64 {
	var total float64
	for i := 1; i < len(pts); i++ {
		d := pts[i].V - pts[i-1].V
		if d < 0 {
			total += pts[i].V
		} else {
			total += d
		}
	}
	return total
}

func populationStddev(vals []float64) float64 {
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / float64(len(vals))
	var variance float64
	for _, v := range vals {
		d := v - mean
		variance += d * d
	}
	return math.Sqrt(variance / float64(len(vals)))
}

// ---------------------------------------------------------------------------
// Aggregation
// ---------------------------------------------------------------------------

// Aggregation reduces samples grouped by a set of labels. Without/by mirror
// PromQL: `by` keeps only the named labels, `without` drops them, and no clause
// collapses everything into a single unlabelled sample (a scalar).
type Aggregation struct {
	Op       string
	Grouping []string
	Without  bool
	Operand  Expression
}

// aggAcc accumulates the running reduction for one group.
type aggAcc struct {
	labels map[string]string
	sum    float64
	min    float64
	max    float64
	count  int
	values []float64 // retained only for stddev
}

func (a *Aggregation) Eval(ctx context.Context, q Querier, at time.Time) (Vector, error) {
	vec, err := a.Operand.Eval(ctx, q, at)
	if err != nil {
		return nil, err
	}
	groups := make(map[string]*aggAcc)
	var order []string
	for _, s := range vec {
		gl := a.groupLabels(s.Labels)
		key := alert.LabelsString(gl)
		g := groups[key]
		if g == nil {
			g = &aggAcc{labels: gl, min: s.Value, max: s.Value}
			groups[key] = g
			order = append(order, key)
		}
		g.sum += s.Value
		g.count++
		if s.Value < g.min {
			g.min = s.Value
		}
		if s.Value > g.max {
			g.max = s.Value
		}
		if a.Op == "stddev" {
			g.values = append(g.values, s.Value)
		}
	}
	sort.Strings(order)
	out := make(Vector, 0, len(order))
	for _, key := range order {
		g := groups[key]
		out = append(out, Sample{Labels: g.labels, Value: a.reduce(g)})
	}
	return out, nil
}

// groupLabels computes the identity of the group a sample falls into. An empty
// result (nil) means the single, scalar-shaped group.
func (a *Aggregation) groupLabels(labels map[string]string) map[string]string {
	if !a.Without && len(a.Grouping) == 0 {
		return nil
	}
	gl := make(map[string]string)
	if a.Without {
		drop := make(map[string]bool, len(a.Grouping))
		for _, k := range a.Grouping {
			drop[k] = true
		}
		for k, v := range labels {
			if !drop[k] {
				gl[k] = v
			}
		}
	} else {
		for _, k := range a.Grouping {
			if v, ok := labels[k]; ok {
				gl[k] = v
			}
		}
	}
	if len(gl) == 0 {
		return nil
	}
	return gl
}

func (a *Aggregation) reduce(g *aggAcc) float64 {
	switch a.Op {
	case "sum":
		return g.sum
	case "avg":
		return g.sum / float64(g.count)
	case "min":
		return g.min
	case "max":
		return g.max
	case "count":
		return float64(g.count)
	case "stddev":
		return populationStddev(g.values)
	}
	return math.NaN()
}

func (a *Aggregation) String() string {
	head := a.Op
	if a.Without {
		head += " without(" + strings.Join(a.Grouping, ", ") + ")"
	} else if len(a.Grouping) > 0 {
		head += " by(" + strings.Join(a.Grouping, ", ") + ")"
	}
	return head + "(" + a.Operand.String() + ")"
}

// quoteString renders a label-matcher value as a double-quoted literal the lexer
// accepts back verbatim, so String output always re-parses.
func quoteString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
