package promexport

import (
	"fmt"
	"math"
	"sync"
	"testing"
)

func TestCounter(t *testing.T) {
	var c Counter
	c.Add(5)
	c.Inc()
	if got := c.Load(); got != 6 {
		t.Errorf("Load() = %d, want 6", got)
	}
}

func TestGauge(t *testing.T) {
	var g Gauge
	g.Set(3.5)
	if got := g.Load(); got != 3.5 {
		t.Errorf("after Set: Load() = %v, want 3.5", got)
	}
	g.Add(1.5)
	if got := g.Load(); got != 5.0 {
		t.Errorf("after Add(1.5): Load() = %v, want 5.0", got)
	}
	g.Add(-2)
	if got := g.Load(); got != 3.0 {
		t.Errorf("after Add(-2): Load() = %v, want 3.0", got)
	}
}

// TestHistogramObserve checks the bucket placement and the cumulative snapshot
// against known observations. le is "less than or equal": a value equal to a
// bound lands in that bound's bucket.
func TestHistogramObserve(t *testing.T) {
	h := NewHistogram([]float64{1, 2, 3})
	// 0.5 -> le1; 1.0 -> le1 (equal goes in); 1.5 -> le2; 2.5 -> le3;
	// 3.5,5 -> +Inf. per-bucket [2,1,1,2]; cumulative [2,3,4,6].
	obs := []float64{0.5, 1.0, 1.5, 2.5, 3.5, 5}
	for _, v := range obs {
		h.Observe(v)
	}
	cumulative, sum, count := h.Snapshot()

	// The full cumulative slice is the assertion that has teeth: it pins every
	// value to the bucket its magnitude selects, so an Observe that files
	// observations into the wrong bucket (e.g. always the first) is caught here.
	// A +Inf-bucket-vs-count check would not: Snapshot derives both from the same
	// running total, so they agree no matter where Observe put the observations.
	wantCum := []uint64{2, 3, 4, 6}
	if !equalU64(cumulative, wantCum) {
		t.Errorf("cumulative = %v, want %v", cumulative, wantCum)
	}
	// count is compared against the number of Observe calls the test made — an
	// independent quantity — not against the +Inf bucket it is computed from.
	if count != uint64(len(obs)) {
		t.Errorf("count = %d, want %d Observe calls", count, len(obs))
	}
	if want := 0.5 + 1.0 + 1.5 + 2.5 + 3.5 + 5; sum != want {
		t.Errorf("sum = %v, want %v", sum, want)
	}
}

// TestHistogramInfAndNaN pins the defined behaviour for the two non-finite
// observations: both land in the +Inf overflow bucket and nowhere else, which
// the full cumulative slice ([0,0,1], not [1,0,0]) asserts — an Observe that
// filed them into the first bucket would fail it. +Inf poisons the sum to +Inf
// and NaN poisons it to NaN, the honest report that a non-finite value was
// observed. count is checked against the single Observe call, not against the
// +Inf bucket it shares a variable with.
func TestHistogramInfAndNaN(t *testing.T) {
	t.Run("+Inf", func(t *testing.T) {
		h := NewHistogram([]float64{1, 2})
		h.Observe(math.Inf(1))
		cumulative, sum, count := h.Snapshot()
		if want := []uint64{0, 0, 1}; !equalU64(cumulative, want) {
			t.Errorf("cumulative = %v, want %v", cumulative, want)
		}
		if count != 1 {
			t.Errorf("count = %d, want 1 Observe call", count)
		}
		if !math.IsInf(sum, 1) {
			t.Errorf("sum = %v, want +Inf", sum)
		}
	})
	t.Run("NaN", func(t *testing.T) {
		h := NewHistogram([]float64{1, 2})
		h.Observe(math.NaN())
		cumulative, sum, count := h.Snapshot()
		if want := []uint64{0, 0, 1}; !equalU64(cumulative, want) {
			t.Errorf("cumulative = %v, want %v", cumulative, want)
		}
		if count != 1 {
			t.Errorf("count = %d, want 1 Observe call", count)
		}
		if !math.IsNaN(sum) {
			t.Errorf("sum = %v, want NaN", sum)
		}
	})
}

func TestNewHistogramPanics(t *testing.T) {
	tests := []struct {
		name   string
		bounds []float64
	}{
		{"unsorted", []float64{1, 3, 2}},
		{"duplicate", []float64{1, 1, 2}},
		{"NaN bound", []float64{1, math.NaN(), 3}},
		{"+Inf bound", []float64{1, 2, math.Inf(1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("NewHistogram(%v) did not panic", tt.bounds)
				}
			}()
			NewHistogram(tt.bounds)
		})
	}
}

func TestDefaultBucketsIsACopy(t *testing.T) {
	want := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	got := DefaultBuckets()
	if len(got) != len(want) {
		t.Fatalf("DefaultBuckets len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("DefaultBuckets = %v, want %v", got, want)
			break
		}
	}
	// A caller mutating the returned slice must not corrupt the defaults.
	got[0] = 999
	if again := DefaultBuckets(); again[0] != 0.005 {
		t.Errorf("DefaultBuckets shares state: second call returned %v", again[0])
	}
}

func TestCounterVecAccumulates(t *testing.T) {
	v := NewCounterVec("hits_total", "", []string{"path"}, 10)
	// The same label values must return the same counter, so increments add up.
	v.WithLabelValues("a").Inc()
	v.WithLabelValues("a").Inc()
	v.WithLabelValues("b").Add(3)
	if got := v.WithLabelValues("a").Load(); got != 2 {
		t.Errorf("path=a counter = %d, want 2", got)
	}
	if got := v.WithLabelValues("b").Load(); got != 3 {
		t.Errorf("path=b counter = %d, want 3", got)
	}
}

func TestCounterVecWrongArityPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("WithLabelValues with wrong arity did not panic")
		}
	}()
	v := NewCounterVec("x", "", []string{"a", "b"}, 10)
	v.WithLabelValues("only-one")
}

// TestCounterVecCardinalityCap is the core of the "an endpoint must never be a
// memory leak driven by request input" guarantee. With maxSeries 3 and ten
// distinct label sets, exactly three series survive: two real and one visible
// __overflow__ series whose count is every folded observation.
func TestCounterVecCardinalityCap(t *testing.T) {
	v := NewCounterVec("req_total", "", []string{"id"}, 3)
	const distinct = 10
	for i := 0; i < distinct; i++ {
		v.WithLabelValues(fmt.Sprintf("id-%d", i)).Inc()
	}

	fams := v.Gather()
	if len(fams) != 1 {
		t.Fatalf("Gather returned %d families, want 1", len(fams))
	}
	samples := fams[0].Samples
	if len(samples) != 3 {
		t.Fatalf("cap not enforced: %d series, want 3", len(samples))
	}

	var overflow *Sample
	var realTotal uint64
	for i := range samples {
		s := &samples[i]
		if s.Labels[0].Value == overflowValue {
			overflow = s
			continue
		}
		realTotal += uint64(s.Value)
	}
	if overflow == nil {
		t.Fatal("no __overflow__ series present")
	}
	// Two real series (one observation each) and the overflow folds the rest.
	if realTotal != 2 {
		t.Errorf("real series total = %d, want 2", realTotal)
	}
	if got := uint64(overflow.Value); got != distinct-2 {
		t.Errorf("overflow count = %d, want %d folded observations", got, distinct-2)
	}
}

// TestHistogramVecGather checks the shape a histogram vec exposes: a bucket per
// bound plus le="+Inf", then sum and count, with the child's observations
// reflected in the counts.
func TestHistogramVecGather(t *testing.T) {
	v := NewHistogramVec("dur_seconds", "", []string{"route"}, []float64{1, 2}, 10)
	v.WithLabelValues("home").Observe(0.5) // le1
	v.WithLabelValues("home").Observe(1.5) // le2
	v.WithLabelValues("home").Observe(3.0) // +Inf

	fams := v.Gather()
	if len(fams) != 1 || fams[0].Type != TypeHistogram {
		t.Fatalf("unexpected families: %+v", fams)
	}
	// Validate guarantees the family is well-formed and renderable.
	if err := fams[0].Validate(); err != nil {
		t.Fatalf("gathered histogram fails Validate: %v", err)
	}

	buckets := map[string]float64{}
	var sum, count float64
	for _, s := range fams[0].Samples {
		switch s.Suffix {
		case suffixBucket:
			le, _ := findLabel(s.Labels, "le")
			buckets[le] = s.Value
		case suffixSum:
			sum = s.Value
		case suffixCount:
			count = s.Value
		}
	}
	if buckets["1"] != 1 || buckets["2"] != 2 || buckets["+Inf"] != 3 {
		t.Errorf("cumulative buckets = %v, want le1=1 le2=2 +Inf=3", buckets)
	}
	if count != 3 {
		t.Errorf("count = %v, want 3", count)
	}
	if sum != 5.0 {
		t.Errorf("sum = %v, want 5.0", sum)
	}
}

// TestCounterVecConcurrent hammers one vec from many goroutines under -race. The
// per-series and grand totals are exact, so a lost update (a non-atomic counter)
// fails the assertion as well as tripping the race detector.
func TestCounterVecConcurrent(t *testing.T) {
	const (
		goroutines = 50
		perG       = 2000
		series     = 10
	)
	v := NewCounterVec("c_total", "", []string{"id"}, 100)

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				v.WithLabelValues(fmt.Sprintf("id-%d", i%series)).Inc()
			}
		}()
	}
	wg.Wait()

	fams := v.Gather()
	var total uint64
	for _, s := range fams[0].Samples {
		total += uint64(s.Value)
		if want := uint64(goroutines * perG / series); uint64(s.Value) != want {
			t.Errorf("series %q = %v, want %d", s.Labels[0].Value, s.Value, want)
		}
	}
	if want := uint64(goroutines * perG); total != want {
		t.Errorf("grand total = %d, want %d", total, want)
	}
}

// TestHistogramVecConcurrent hammers histogram children under -race and checks
// the counts and sum are exact after the join.
func TestHistogramVecConcurrent(t *testing.T) {
	const (
		goroutines = 40
		perG       = 1000
		series     = 4
		obsValue   = 0.42
	)
	v := NewHistogramVec("h_seconds", "", []string{"id"}, DefaultBuckets(), 100)

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				v.WithLabelValues(fmt.Sprintf("id-%d", i%series)).Observe(obsValue)
			}
		}()
	}
	wg.Wait()

	fams := v.Gather()
	var totalCount float64
	for _, s := range fams[0].Samples {
		if s.Suffix != suffixCount {
			continue
		}
		totalCount += s.Value
		wantPer := float64(goroutines * perG / series)
		if s.Value != wantPer {
			t.Errorf("series %q count = %v, want %v", s.Labels[0].Value, s.Value, wantPer)
		}
	}
	if want := float64(goroutines * perG); totalCount != want {
		t.Errorf("total count = %v, want %v", totalCount, want)
	}
	// The +Inf bucket of each series must equal that series' count.
	perSeries := map[string][2]float64{} // id -> [infBucket, count]
	for _, s := range fams[0].Samples {
		id := s.Labels[0].Value
		e := perSeries[id]
		switch {
		case s.Suffix == suffixBucket && labelValue(s.Labels, "le") == "+Inf":
			e[0] = s.Value
		case s.Suffix == suffixCount:
			e[1] = s.Value
		}
		perSeries[id] = e
	}
	for id, e := range perSeries {
		if e[0] != e[1] {
			t.Errorf("series %q: +Inf bucket %v != count %v", id, e[0], e[1])
		}
	}
}

func labelValue(labels []Label, name string) string {
	v, _ := findLabel(labels, name)
	return v
}

func equalU64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
