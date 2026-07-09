package testutil

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"metrics-system/internal/model"
)

// The leak detector is the one helper that can silently stop working: if its
// parser stops recognising the runtime's dump format, it reports zero leaks
// forever and every test that relies on it becomes decorative. So it gets tests
// of its own, in both directions.

func TestFindLeaksReportsAGoroutineStartedAfterTheSnapshot(t *testing.T) {
	before := goroutineIDs()

	block := make(chan struct{})
	started := make(chan struct{})
	go func() {
		close(started)
		<-block // the leak: nothing will ever send
	}()
	<-started

	leaked := findLeaks(before, 50*time.Millisecond)
	if len(leaked) != 1 {
		t.Fatalf("want exactly 1 leaked goroutine, got %d:\n%s", len(leaked), strings.Join(leaked, "\n"))
	}
	if !strings.Contains(leaked[0], "TestFindLeaksReportsAGoroutineStartedAfterTheSnapshot") {
		t.Errorf("leak report should name the goroutine's creator, got:\n%s", leaked[0])
	}

	close(block)
}

func TestFindLeaksIgnoresAGoroutineThatFinishes(t *testing.T) {
	before := goroutineIDs()

	done := make(chan struct{})
	go close(done)
	<-done

	if leaked := findLeaks(before, time.Second); len(leaked) != 0 {
		t.Fatalf("a finished goroutine is not a leak, got:\n%s", strings.Join(leaked, "\n"))
	}
}

// findLeaks must tolerate a goroutine that is on its way out: the test's
// cleanup runs the instant the last channel send returns, often before the
// receiving goroutine has been descheduled. That is what the retry loop buys.
func TestFindLeaksWaitsForAGoroutineToExit(t *testing.T) {
	before := goroutineIDs()

	stop := make(chan struct{})
	go func() { <-stop }()
	close(stop)

	if leaked := findLeaks(before, time.Second); len(leaked) != 0 {
		t.Fatalf("goroutine had time to exit, got:\n%s", strings.Join(leaked, "\n"))
	}
}

func TestFindLeaksIgnoresRuntimeOwnedGoroutines(t *testing.T) {
	// Every goroutine currently alive belongs to the runtime or the testing
	// package, so an empty "before" set must still yield no leaks.
	if leaked := findLeaks(map[uint64]bool{}, 50*time.Millisecond); len(leaked) != 0 {
		for _, l := range leaked {
			// The test binary's own goroutine is in there; anything else is a
			// gap in ignoredFrames.
			if !strings.Contains(l, "testutil.TestFindLeaksIgnoresRuntimeOwnedGoroutines") &&
				!strings.Contains(l, "testutil.goroutines") {
				t.Errorf("unexpected goroutine not covered by ignoredFrames:\n%s", l)
			}
		}
	}
}

func TestParseGoroutineHeader(t *testing.T) {
	tests := []struct {
		name   string
		block  string
		wantID uint64
		wantOK bool
	}{
		{
			name:   "running",
			block:  "goroutine 1 [running]:\nmain.main()",
			wantID: 1,
			wantOK: true,
		},
		{
			name:   "blocked with duration",
			block:  "goroutine 4729 [chan receive, 95 minutes]:\nfoo.bar()",
			wantID: 4729,
			wantOK: true,
		},
		{
			name:   "not a goroutine block",
			block:  "some other text",
			wantOK: false,
		},
		{
			name:   "malformed id",
			block:  "goroutine abc [running]:\nx()",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, ok := parseGoroutine(tt.block)
			if ok != tt.wantOK {
				t.Fatalf("ok: want %v, got %v", tt.wantOK, ok)
			}
			if ok && g.id != tt.wantID {
				t.Errorf("id: want %d, got %d", tt.wantID, g.id)
			}
		})
	}
}

// goroutines() must not truncate: a dump larger than the initial buffer is the
// exact case where a leak detector is most needed, and a truncated dump silently
// loses goroutines off the end.
func TestGoroutinesGrowsItsBuffer(t *testing.T) {
	const n = 400
	stop := make(chan struct{})
	ready := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			ready <- struct{}{}
			<-stop
		}()
	}
	for i := 0; i < n; i++ {
		<-ready
	}
	defer close(stop)

	got := len(goroutines())
	if got < n {
		t.Fatalf("dump lost goroutines: want at least %d, got %d", n, got)
	}
}

func TestEventuallySucceedsAsSoonAsTheConditionHolds(t *testing.T) {
	var flips atomic.Int64
	start := time.Now()
	Eventually(t, time.Second, 5*time.Millisecond, func() bool {
		return flips.Add(1) >= 3
	}, "counter should reach 3")

	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("Eventually should return on the first true condition, took %s", elapsed)
	}
}

func TestEventuallyDoesNotPayATickWhenAlreadyTrue(t *testing.T) {
	start := time.Now()
	Eventually(t, time.Second, 200*time.Millisecond, func() bool { return true }, "always true")
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("Eventually waited for a tick before its first check, took %s", elapsed)
	}
}

func TestBuilderCopiesLabelsPerBuild(t *testing.T) {
	b := NewMetric().WithLabel("host", "web-1")

	first := b.Build()
	second := b.Build()
	second.Labels["host"] = "web-2"

	if first.Labels["host"] != "web-1" {
		t.Fatalf("two Build calls alias one label map: first=%v", first.Labels)
	}
}

func TestBuilderEmptyLabelsBecomeNil(t *testing.T) {
	m := NewMetric().Build()
	if m.Labels != nil {
		t.Errorf("want nil labels for an unlabelled metric, got %v", m.Labels)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("the zero builder must produce a valid metric: %v", err)
	}
}

func TestBuilderDefaultsAreAValidMetric(t *testing.T) {
	batch := NewBatch().WithMetrics(NewMetric().Build()).Build()
	if err := batch.Validate(); err != nil {
		t.Fatalf("the zero batch must be valid: %v", err)
	}
}

func TestWithSeriesSpacesPointsByStep(t *testing.T) {
	batch := NewBatch().WithSeries("cpu", 3, time.Minute, func(i int) float64 { return float64(i * 10) }).Build()

	if len(batch.Metrics) != 3 {
		t.Fatalf("want 3 metrics, got %d", len(batch.Metrics))
	}
	for i, m := range batch.Metrics {
		wantTS := BaseTime.Add(time.Duration(i) * time.Minute)
		if !m.Timestamp.Equal(wantTS) {
			t.Errorf("metric %d timestamp: want %s, got %s", i, wantTS, m.Timestamp)
		}
		if want := float64(i * 10); m.Value != want {
			t.Errorf("metric %d value: want %v, got %v", i, want, m.Value)
		}
	}
}

func TestNormalizeReplacesVolatileValues(t *testing.T) {
	in := "fired at 2026-07-09T12:30:00Z fingerprint a3f9c1d2e4b5a6f7 after 1.5s"
	want := "fired at <TIMESTAMP> fingerprint <ID> after <DURATION>"
	if got := Normalize(in); got != want {
		t.Errorf("Normalize:\nwant %q\ngot  %q", want, got)
	}
}

func TestAssertLabelsEqualTreatsNilAndEmptyAsEqual(t *testing.T) {
	// A recording TB would be better, but testing.TB cannot be implemented
	// outside the testing package. Assert on the observable behaviour instead:
	// this must not fail the surrounding test.
	AssertLabelsEqual(t, nil, map[string]string{})
	AssertLabelsEqual(t, map[string]string{}, nil)
}

func TestAssertMetricEqualComparesTimestampsByInstant(t *testing.T) {
	utc := model.Metric{Name: "cpu", Timestamp: BaseTime}
	msk := model.Metric{Name: "cpu", Timestamp: BaseTime.In(time.FixedZone("MSK", 3*60*60))}
	AssertMetricEqual(t, utc, msk) // same instant, different location
}
