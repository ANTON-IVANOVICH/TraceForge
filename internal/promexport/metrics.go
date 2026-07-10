package promexport

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
)

// Counter is a monotonically increasing count backed by a single atomic word.
// There is deliberately no Sub: a counter that goes down reads to a rate() query
// as a reset, which invents a spike that never happened.
type Counter struct {
	v atomic.Uint64
}

// Add increases the counter by delta.
func (c *Counter) Add(delta uint64) { c.v.Add(delta) }

// Inc increases the counter by one.
func (c *Counter) Inc() { c.v.Add(1) }

// Load returns the current value.
func (c *Counter) Load() uint64 { return c.v.Load() }

// Gauge is a float that may move in either direction. The value lives as its
// IEEE-754 bits in one atomic word, so a concurrent Set and Load can never tear
// a 64-bit value into a half-written number.
type Gauge struct {
	bits atomic.Uint64
}

// Set replaces the value.
func (g *Gauge) Set(v float64) { g.bits.Store(math.Float64bits(v)) }

// Load returns the current value.
func (g *Gauge) Load() float64 { return math.Float64frombits(g.bits.Load()) }

// Add adds v to the value. A read-modify-write on a float is not a single atomic
// add, so it retries a compare-and-swap until no other writer raced in between.
// Contention is expected to be low: gauges are set far less often than counters
// are incremented.
func (g *Gauge) Add(v float64) {
	for {
		old := g.bits.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if g.bits.CompareAndSwap(old, next) {
			return
		}
	}
}

// Histogram counts observations into buckets whose upper bounds are fixed at
// construction. Counts are stored per bucket, not cumulatively: an Observe
// touches exactly one bucket and so stays a single atomic add no matter how many
// buckets there are. Snapshot turns the per-bucket deltas into the cumulative
// counts the exposition format wants.
type Histogram struct {
	bounds  []float64       // ascending, finite, immutable after NewHistogram
	counts  []atomic.Uint64 // len(bounds)+1; the last is the implicit +Inf bucket
	sumBits atomic.Uint64
}

// NewHistogram returns a histogram with the given bucket upper bounds, which
// must be strictly ascending and finite; the +Inf bucket is implicit and must
// not be listed. It panics on unsorted, NaN or infinite bounds — those are a
// programming error in the metric's definition, caught once at init rather than
// silently miscounting on every Observe.
func NewHistogram(bounds []float64) *Histogram {
	for i, b := range bounds {
		switch {
		case math.IsNaN(b):
			panic("promexport: histogram bound is NaN")
		case math.IsInf(b, 0):
			panic("promexport: histogram bound is infinite; the +Inf bucket is implicit")
		case i > 0 && b <= bounds[i-1]:
			panic("promexport: histogram bounds must be strictly ascending")
		}
	}
	return &Histogram{
		bounds: append([]float64(nil), bounds...),
		counts: make([]atomic.Uint64, len(bounds)+1),
	}
}

// Observe records one value.
//
// A NaN cannot be ordered against any bound. It is routed to the +Inf overflow
// bucket, which keeps the invariant that the cumulative +Inf bucket equals the
// total count — the histogram stays well-formed even for garbage input. The sum
// it turns to NaN is the honest signal that a non-number was observed, rather
// than a quietly wrong finite total. (SearchFloat64s already lands NaN in the
// last bucket, since its predicate bound>=NaN is never true; the explicit branch
// states the invariant instead of leaving it to a library edge case.)
func (h *Histogram) Observe(v float64) {
	idx := len(h.bounds)
	if !math.IsNaN(v) {
		idx = sort.SearchFloat64s(h.bounds, v)
	}
	h.counts[idx].Add(1)

	for {
		old := h.sumBits.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if h.sumBits.CompareAndSwap(old, next) {
			return
		}
	}
}

// Snapshot returns the cumulative bucket counts (one per bound plus a final
// +Inf entry), the sum of observations, and the total count.
//
// count is the running total of the buckets, not a separate counter, so the
// +Inf bucket (the last cumulative entry) equals count by construction — even
// while observations race in, the two can never disagree.
func (h *Histogram) Snapshot() (cumulative []uint64, sum float64, count uint64) {
	cumulative = make([]uint64, len(h.counts))
	var running uint64
	for i := range h.counts {
		running += h.counts[i].Load()
		cumulative[i] = running
	}
	return cumulative, math.Float64frombits(h.sumBits.Load()), running
}

// DefaultBuckets returns the default latency buckets, in seconds. A fresh slice
// is returned each call so a caller that mutates it cannot corrupt the defaults
// another caller sees.
func DefaultBuckets() []float64 {
	return []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
}

// overflowValue is the label value every dimension of the overflow series
// carries. It is deliberately conspicuous: when a Vec's labelling is unbounded,
// the folded series must be visible on the dashboard, because a silent drop
// hides the fact that the labels are wrong.
const overflowValue = "__overflow__"

// vec is the machinery shared by CounterVec and HistogramVec: a mutex-guarded
// map from an injective label-value key to a child metric, with a hard cap on
// the number of distinct series a metrics endpoint may hold. Without the cap,
// request-derived labels turn the endpoint into a memory leak driven by input.
type vec[T any] struct {
	name       string
	help       string
	labelNames []string
	maxSeries  int
	newChild   func() *T

	mu          sync.Mutex
	series      map[string]*vecEntry[T]
	overflowKey string
}

type vecEntry[T any] struct {
	values []string
	child  *T
}

func newVec[T any](name, help string, labelNames []string, maxSeries int, newChild func() *T) vec[T] {
	if maxSeries < 1 {
		maxSeries = 1
	}
	overflow := make([]string, len(labelNames))
	for i := range overflow {
		overflow[i] = overflowValue
	}
	return vec[T]{
		name:        name,
		help:        help,
		labelNames:  append([]string(nil), labelNames...),
		maxSeries:   maxSeries,
		newChild:    newChild,
		series:      make(map[string]*vecEntry[T]),
		overflowKey: valuesKey(overflow),
	}
}

// childFor returns the child for a label-value tuple, creating it on first use.
// It panics on the wrong number of values: that is a programming error at the
// call site, not runtime input, so it must fail loudly rather than fabricate a
// series.
func (v *vec[T]) childFor(vals []string) *T {
	if len(vals) != len(v.labelNames) {
		panic(fmt.Sprintf("promexport: %s takes %d label values, got %d", v.name, len(v.labelNames), len(vals)))
	}
	key := valuesKey(vals)

	v.mu.Lock()
	defer v.mu.Unlock()

	if e, ok := v.series[key]; ok {
		return e.child
	}
	// The final slot is reserved for the overflow series, so that once the map is
	// full there is always somewhere to fold a new label set. The effective
	// capacity for real series is therefore maxSeries-1; the last is the
	// __overflow__ series, counted in the cap, never exceeded.
	if len(v.series) < v.maxSeries-1 {
		e := &vecEntry[T]{values: append([]string(nil), vals...), child: v.newChild()}
		v.series[key] = e
		return e.child
	}
	return v.overflowChild()
}

func (v *vec[T]) overflowChild() *T {
	if e, ok := v.series[v.overflowKey]; ok {
		return e.child
	}
	vals := make([]string, len(v.labelNames))
	for i := range vals {
		vals[i] = overflowValue
	}
	e := &vecEntry[T]{values: vals, child: v.newChild()}
	v.series[v.overflowKey] = e
	return e.child
}

// snapshot copies the current entries so Gather can render without holding the
// lock across the whole exposition and blocking observers.
func (v *vec[T]) snapshot() []*vecEntry[T] {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]*vecEntry[T], 0, len(v.series))
	for _, e := range v.series {
		out = append(out, e)
	}
	return out
}

// CounterVec is a family of counters distinguished by label values, capped at a
// fixed number of series.
type CounterVec struct {
	vec[Counter]
}

// NewCounterVec returns a CounterVec. maxSeries bounds the number of distinct
// label combinations it will ever hold; beyond it, values fold into a single
// visible __overflow__ series.
func NewCounterVec(name, help string, labelNames []string, maxSeries int) *CounterVec {
	return &CounterVec{vec: newVec(name, help, labelNames, maxSeries, func() *Counter { return &Counter{} })}
}

// WithLabelValues returns the counter for the given label values, creating it on
// first use. It panics if the number of values does not match the label names.
func (v *CounterVec) WithLabelValues(vals ...string) *Counter {
	return v.childFor(vals)
}

// Gather renders the vec as a single counter family.
func (v *CounterVec) Gather() []Family {
	entries := v.snapshot()
	samples := make([]Sample, 0, len(entries))
	for _, e := range entries {
		samples = append(samples, Sample{
			Labels: labelsFor(v.labelNames, e.values),
			Value:  float64(e.child.Load()),
		})
	}
	return []Family{{Name: v.name, Help: v.help, Type: TypeCounter, Samples: samples}}
}

// HistogramVec is a family of histograms distinguished by label values, capped
// at a fixed number of series. Every child shares the same bucket bounds.
type HistogramVec struct {
	vec[Histogram]
	bounds []float64
}

// NewHistogramVec returns a HistogramVec. The bounds are validated once here
// (via NewHistogram, which panics on bad bounds) so the programming error is
// caught at init rather than on the first Observe.
func NewHistogramVec(name, help string, labelNames []string, bounds []float64, maxSeries int) *HistogramVec {
	b := append([]float64(nil), bounds...)
	_ = NewHistogram(b) // panics on unsorted/NaN/infinite bounds
	return &HistogramVec{
		vec:    newVec(name, help, labelNames, maxSeries, func() *Histogram { return NewHistogram(b) }),
		bounds: b,
	}
}

// WithLabelValues returns the histogram for the given label values, creating it
// on first use. It panics if the number of values does not match the label
// names.
func (v *HistogramVec) WithLabelValues(vals ...string) *Histogram {
	return v.childFor(vals)
}

// Gather renders the vec as a single histogram family: for each series a bucket
// sample per bound plus the mandatory le="+Inf" bucket, then the sum and count.
func (v *HistogramVec) Gather() []Family {
	entries := v.snapshot()
	var samples []Sample
	for _, e := range entries {
		cumulative, sum, count := e.child.Snapshot()
		base := labelsFor(v.labelNames, e.values)
		for i, c := range cumulative {
			le := "+Inf"
			if i < len(v.bounds) {
				le = formatValue(v.bounds[i])
			}
			samples = append(samples, Sample{
				Suffix: suffixBucket,
				Labels: appendLabel(base, "le", le),
				Value:  float64(c),
			})
		}
		samples = append(samples,
			Sample{Suffix: suffixSum, Labels: base, Value: sum},
			Sample{Suffix: suffixCount, Labels: base, Value: float64(count)},
		)
	}
	return []Family{{Name: v.name, Help: v.help, Type: TypeHistogram, Samples: samples}}
}

// valuesKey is the injective map key for a label-value tuple. Every value is
// followed by an escaped separator, so two different tuples can never collide —
// the same reasoning as serieskey.go, where an unescaped join silently merges
// series the moment a value contains the separator.
func valuesKey(vals []string) string {
	var buf []byte
	for _, v := range vals {
		buf = appendField(buf, v)
	}
	return string(buf)
}

func labelsFor(names, values []string) []Label {
	ls := make([]Label, len(names))
	for i := range names {
		ls[i] = Label{Name: names[i], Value: values[i]}
	}
	return ls
}

// appendLabel returns base with one label appended, without aliasing base, so
// each bucket sample gets its own le without disturbing the shared base used by
// the sum and count samples.
func appendLabel(base []Label, name, value string) []Label {
	out := make([]Label, len(base)+1)
	copy(out, base)
	out[len(base)] = Label{Name: name, Value: value}
	return out
}
