package promexport

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

// TestWriteGolden pins the exact bytes of a rendered scrape. It is the contract
// with `promtool check metrics`: any change to spacing, ordering or escaping
// changes this string, so a regression cannot slip through as "still parses".
func TestWriteGolden(t *testing.T) {
	families := []Family{
		{
			Name: "requests_total", Help: "total requests", Type: TypeCounter,
			Samples: []Sample{{Value: 42}},
		},
		{
			// Labels are supplied out of order to prove Write sorts them.
			Name: "temperature_celsius", Type: TypeGauge,
			Samples: []Sample{{
				Labels: []Label{{"zone", "north"}, {"building", "hq"}},
				Value:  21.5,
			}},
		},
		{
			// Bucket/sum/count supplied shuffled to prove Write orders them:
			// buckets ascending by le with +Inf last, then sum, then count.
			Name: "latency_seconds", Type: TypeHistogram,
			Samples: []Sample{
				{Suffix: suffixSum, Value: 0.7},
				{Suffix: suffixBucket, Labels: []Label{{"le", "+Inf"}}, Value: 1},
				{Suffix: suffixCount, Value: 1},
				{Suffix: suffixBucket, Labels: []Label{{"le", "1"}}, Value: 1},
				{Suffix: suffixBucket, Labels: []Label{{"le", "0.5"}}, Value: 0},
				{Suffix: suffixBucket, Labels: []Label{{"le", "2.5"}}, Value: 1},
			},
		},
		{
			// Samples supplied out of value order; sorted by label value.
			Name: "special", Type: TypeGauge,
			Samples: []Sample{
				{Labels: []Label{{"k", "pos"}}, Value: math.Inf(1)},
				{Labels: []Label{{"k", "nan"}}, Value: math.NaN()},
				{Labels: []Label{{"k", "neg"}}, Value: math.Inf(-1)},
			},
		},
		{
			// Help holds a backslash, a newline and a double quote. In HELP the
			// quote is NOT escaped; the backslash and newline are.
			Name: "helpy", Help: "a\\b\nc\"d", Type: TypeCounter,
			Samples: []Sample{{Value: 1}},
		},
		{
			// A label value holds a backslash, a newline and a double quote. All
			// three are escaped, because all three would otherwise break the line.
			Name: "labely", Type: TypeCounter,
			Samples: []Sample{{Labels: []Label{{"k", "a\\b\nc\"d"}}, Value: 1}},
		},
	}

	want := `# HELP requests_total total requests
# TYPE requests_total counter
requests_total 42
# TYPE temperature_celsius gauge
temperature_celsius{building="hq",zone="north"} 21.5
# TYPE latency_seconds histogram
latency_seconds_bucket{le="0.5"} 0
latency_seconds_bucket{le="1"} 1
latency_seconds_bucket{le="2.5"} 1
latency_seconds_bucket{le="+Inf"} 1
latency_seconds_sum 0.7
latency_seconds_count 1
# TYPE special gauge
special{k="nan"} NaN
special{k="neg"} -Inf
special{k="pos"} +Inf
# HELP helpy a\\b\nc"d
# TYPE helpy counter
helpy 1
# TYPE labely counter
labely{k="a\\b\nc\"d"} 1
`

	var buf bytes.Buffer
	if err := Write(&buf, families); err != nil {
		t.Fatalf("Write returned error for valid families: %v", err)
	}
	if got := buf.String(); got != want {
		t.Errorf("Write output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestWriteSkipsInvalidButKeepsRest proves the "one bad metric never blanks a
// scrape" contract: a family that fails Validate is dropped, the others are
// still rendered, and Write reports the first skip.
func TestWriteSkipsInvalidButKeepsRest(t *testing.T) {
	families := []Family{
		{Name: "1bad", Type: TypeCounter, Samples: []Sample{{Value: 1}}}, // leading digit
		{Name: "good", Type: TypeCounter, Samples: []Sample{{Value: 2}}},
	}
	var buf bytes.Buffer
	err := Write(&buf, families)
	if err == nil {
		t.Fatal("Write returned nil error; want the skipped family reported")
	}
	got := buf.String()
	if strings.Contains(got, "1bad") {
		t.Errorf("output contains the invalid family:\n%s", got)
	}
	if !strings.Contains(got, "good 2\n") {
		t.Errorf("output dropped the valid family:\n%s", got)
	}
}

// TestWriteHistogramLeRendersLast pins the "le renders last" invariant with
// exact bytes. prepare() holds le apart and appends it after the sorted
// non-le labels; the golden output is only sensitive to that when a bucket
// carries a label that sorts AFTER le ("route", 'r' > 'l') — leaving le in its
// alphabetical slot would emit {code,le,route} instead of {code,route,le}. A
// label that sorts BEFORE le ("code", 'c' < 'l') stays put either way, and is
// present to prove the rule reorders only what must move. The _sum and _count
// lines carry the same non-le labels and no le at all.
func TestWriteHistogramLeRendersLast(t *testing.T) {
	families := []Family{{
		Name: "http_request_duration_seconds", Type: TypeHistogram,
		Samples: []Sample{
			// Labels supplied out of order; le must land last after sorting.
			{Suffix: suffixBucket, Labels: []Label{{"route", "home"}, {"le", "1"}, {"code", "200"}}, Value: 2},
			{Suffix: suffixBucket, Labels: []Label{{"le", "+Inf"}, {"code", "200"}, {"route", "home"}}, Value: 3},
			{Suffix: suffixSum, Labels: []Label{{"route", "home"}, {"code", "200"}}, Value: 4.5},
			{Suffix: suffixCount, Labels: []Label{{"code", "200"}, {"route", "home"}}, Value: 3},
		},
	}}

	want := `# TYPE http_request_duration_seconds histogram
http_request_duration_seconds_bucket{code="200",route="home",le="1"} 2
http_request_duration_seconds_bucket{code="200",route="home",le="+Inf"} 3
http_request_duration_seconds_sum{code="200",route="home"} 4.5
http_request_duration_seconds_count{code="200",route="home"} 3
`

	var buf bytes.Buffer
	if err := Write(&buf, families); err != nil {
		t.Fatalf("Write returned error for a valid histogram: %v", err)
	}
	if got := buf.String(); got != want {
		t.Errorf("le-last invariant broken\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

// TestWriteRejectsDuplicateFamilyName covers the cross-family duplicate-name
// guard: a metric name may carry only one TYPE line in the whole document, so a
// second family with the same name is dropped and reported, while the first and
// any differently-named family still render.
func TestWriteRejectsDuplicateFamilyName(t *testing.T) {
	families := []Family{
		{Name: "dup_total", Type: TypeCounter, Samples: []Sample{{Value: 1}}},
		{Name: "dup_total", Type: TypeCounter, Samples: []Sample{{Value: 2}}},
		{Name: "other_total", Type: TypeCounter, Samples: []Sample{{Value: 3}}},
	}
	var buf bytes.Buffer
	err := Write(&buf, families)
	if err == nil {
		t.Fatal("Write returned nil error; want the duplicate family reported")
	}
	if !strings.Contains(err.Error(), "dup_total") {
		t.Errorf("error does not name the duplicated metric: %v", err)
	}
	got := buf.String()
	if n := strings.Count(got, "# TYPE dup_total "); n != 1 {
		t.Errorf("dup_total rendered %d TYPE lines, want exactly 1:\n%s", n, got)
	}
	if !strings.Contains(got, "other_total 3\n") {
		t.Errorf("a differently-named family was dropped:\n%s", got)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		family  Family
		wantErr bool
	}{
		{
			name:   "ok counter",
			family: Family{Name: "http_requests_total", Type: TypeCounter, Samples: []Sample{{Value: 1}}},
		},
		{
			name:    "empty name",
			family:  Family{Name: "", Type: TypeCounter},
			wantErr: true,
		},
		{
			name:    "leading digit",
			family:  Family{Name: "1requests", Type: TypeCounter},
			wantErr: true,
		},
		{
			name:    "dash in name",
			family:  Family{Name: "http-requests", Type: TypeCounter},
			wantErr: true,
		},
		{
			name:    "colon in label name",
			family:  Family{Name: "m", Type: TypeGauge, Samples: []Sample{{Labels: []Label{{"le:x", "1"}}, Value: 1}}},
			wantErr: true,
		},
		{
			name:    "duplicate label in one sample",
			family:  Family{Name: "m", Type: TypeGauge, Samples: []Sample{{Labels: []Label{{"a", "1"}, {"a", "2"}}, Value: 1}}},
			wantErr: true,
		},
		{
			name:    "duplicate series",
			family:  Family{Name: "m", Type: TypeGauge, Samples: []Sample{{Labels: []Label{{"a", "1"}}}, {Labels: []Label{{"a", "1"}}}}},
			wantErr: true,
		},
		{
			name:    "histogram bucket without le",
			family:  Family{Name: "m", Type: TypeHistogram, Samples: []Sample{{Suffix: suffixBucket, Value: 1}}},
			wantErr: true,
		},
		{
			name:    "histogram bucket with non-numeric le",
			family:  Family{Name: "m", Type: TypeHistogram, Samples: []Sample{{Suffix: suffixBucket, Labels: []Label{{"le", "big"}}, Value: 1}}},
			wantErr: true,
		},
		{
			name:   "histogram bucket with +Inf le is fine",
			family: Family{Name: "m", Type: TypeHistogram, Samples: []Sample{{Suffix: suffixBucket, Labels: []Label{{"le", "+Inf"}}, Value: 1}}},
		},
		{
			name:    "counter with a suffix",
			family:  Family{Name: "m", Type: TypeCounter, Samples: []Sample{{Suffix: suffixSum, Value: 1}}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.family.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidMetricName(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"", false},
		{"http_requests_total", true},
		{"_underscore", true},
		{":colon_start", true},
		{"has:colon", true},
		{"1leading_digit", false},
		{"has-dash", false},
		{"has space", false},
		{"trailing_digit9", true},
		{"a9:_", true},
	}
	for _, tt := range tests {
		if got := ValidMetricName(tt.s); got != tt.want {
			t.Errorf("ValidMetricName(%q) = %v, want %v", tt.s, got, tt.want)
		}
	}
}

func TestValidLabelName(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"", false},
		{"le", true},
		{"_x", true},
		{"method", true},
		{"m9", true},
		{"1bad", false},
		{"has-dash", false},
		// The one difference from a metric name: a colon is not a valid label name.
		{"has:colon", false},
		{":", false},
	}
	for _, tt := range tests {
		if got := ValidLabelName(tt.s); got != tt.want {
			t.Errorf("ValidLabelName(%q) = %v, want %v", tt.s, got, tt.want)
		}
	}
}
