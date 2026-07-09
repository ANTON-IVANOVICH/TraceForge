package testutil

import (
	"maps"
	"testing"
	"time"

	"metrics-system/internal/model"
)

// AssertMetricEqual compares two metrics field by field, reporting which field
// differs. A bare reflect.DeepEqual would say "not equal" and leave the reader
// to diff two multi-line structs by eye.
//
// Timestamps are compared with time.Time.Equal, not ==: the same instant in two
// locations, or with and without a monotonic reading, is the same timestamp but
// not the same struct.
func AssertMetricEqual(tb testing.TB, want, got model.Metric) {
	tb.Helper()
	if want.Name != got.Name {
		tb.Errorf("metric name: want %q, got %q", want.Name, got.Name)
	}
	if want.Type != got.Type {
		tb.Errorf("metric type: want %s, got %s", want.Type, got.Type)
	}
	if want.Value != got.Value {
		tb.Errorf("metric value: want %v, got %v", want.Value, got.Value)
	}
	if !want.Timestamp.Equal(got.Timestamp) {
		tb.Errorf("metric timestamp: want %s, got %s",
			want.Timestamp.Format(time.RFC3339Nano), got.Timestamp.Format(time.RFC3339Nano))
	}
	AssertLabelsEqual(tb, want.Labels, got.Labels)
}

// AssertLabelsEqual compares label sets, treating nil and empty as equal — the
// storage layer normalises an empty map to nil, and no test should care.
func AssertLabelsEqual(tb testing.TB, want, got map[string]string) {
	tb.Helper()
	if len(want) == 0 && len(got) == 0 {
		return
	}
	if !maps.Equal(want, got) {
		tb.Errorf("labels: want %v, got %v", want, got)
	}
}

// AssertMetricsEqual compares two slices element-wise after checking length, so
// a length mismatch does not cascade into an index-out-of-range panic.
func AssertMetricsEqual(tb testing.TB, want, got []model.Metric) {
	tb.Helper()
	if len(want) != len(got) {
		tb.Fatalf("metric count: want %d, got %d\nwant: %v\ngot:  %v", len(want), len(got), want, got)
	}
	for i := range want {
		AssertMetricEqual(tb, want[i], got[i])
	}
}

// AssertNoError fails the test immediately when err is non-nil. It exists to
// keep the happy path of a test readable; the alternative is a four-line if
// after every call.
func AssertNoError(tb testing.TB, err error, msg string, args ...any) {
	tb.Helper()
	if err != nil {
		tb.Fatalf("%s: unexpected error: %v", sprintf(msg, args...), err)
	}
}

// AssertError fails when err is nil, or when its message does not contain
// substr. Matching a substring rather than the exact error keeps the test from
// breaking when someone fixes a typo in a message; matching *something* keeps it
// from passing when an unrelated error is returned.
func AssertError(tb testing.TB, err error, substr string) {
	tb.Helper()
	if err == nil {
		tb.Fatalf("want error containing %q, got nil", substr)
	}
	if substr != "" && !contains(err.Error(), substr) {
		tb.Fatalf("error %q does not contain %q", err, substr)
	}
}

// WithinDuration asserts want and got are no more than delta apart, for wall
// clocks that tests cannot pin down exactly.
func WithinDuration(tb testing.TB, want, got time.Time, delta time.Duration, msg string, args ...any) {
	tb.Helper()
	diff := want.Sub(got)
	if diff < 0 {
		diff = -diff
	}
	if diff > delta {
		tb.Errorf("%s: timestamps differ by %s (max %s): want %s, got %s",
			sprintf(msg, args...), diff, delta, want, got)
	}
}
