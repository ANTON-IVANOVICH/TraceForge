package storage

import (
	"maps"
	"strings"
	"testing"
)

func TestSeriesKeyIsCanonical(t *testing.T) {
	t.Parallel()

	k1 := SeriesKey("cpu", map[string]string{"b": "2", "a": "1"})
	k2 := SeriesKey("cpu", map[string]string{"a": "1", "b": "2"})
	if k1 != k2 {
		t.Fatalf("label order must not affect the key: %q vs %q", k1, k2)
	}
	if want := "cpu{a=1,b=2}"; k1 != want {
		t.Errorf("key: want %q, got %q", want, k1)
	}
	if k := SeriesKey("cpu", nil); k != "cpu" {
		t.Errorf("unlabelled key: want %q, got %q", "cpu", k)
	}
	if k := SeriesKey("cpu", map[string]string{}); k != "cpu" {
		t.Errorf("empty label map must equal nil labels, got %q", k)
	}
}

// These are the collisions the unescaped encoding admitted. Each pair is two
// genuinely different series that shared one key, which meant their points were
// appended to the same slice and the stored label set was whichever writer
// arrived first.
func TestSeriesKeyDistinctSeriesNeverShareAKey(t *testing.T) {
	t.Parallel()

	type series struct {
		name   string
		labels map[string]string
	}
	pairs := []struct {
		name string
		a, b series
	}{
		{
			name: "comma inside a label value",
			a:    series{"cpu", map[string]string{"a": "b,c=d"}},
			b:    series{"cpu", map[string]string{"a": "b", "c": "d"}},
		},
		{
			name: "equals sign inside a label value",
			a:    series{"cpu", map[string]string{"a": "b=c"}},
			b:    series{"cpu", map[string]string{"a=b": "c"}},
		},
		{
			name: "label section inside the metric name",
			a:    series{"cpu{a=b}", nil},
			b:    series{"cpu", map[string]string{"a": "b"}},
		},
		{
			name: "closing brace inside a label value",
			a:    series{"cpu", map[string]string{"a": "b}"}},
			b:    series{"cpu", map[string]string{"a": "b"}},
		},
		{
			name: "backslash lets a value impersonate an escape",
			a:    series{"cpu", map[string]string{"a": `b\`}},
			b:    series{"cpu", map[string]string{"a": `b`, `\`: ``}},
		},
	}

	for _, tt := range pairs {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ka := SeriesKey(tt.a.name, tt.a.labels)
			kb := SeriesKey(tt.b.name, tt.b.labels)
			if ka == kb {
				t.Fatalf("distinct series share key %q:\n  (%q, %v)\n  (%q, %v)",
					ka, tt.a.name, tt.a.labels, tt.b.name, tt.b.labels)
			}
		})
	}
}

func TestSeriesKeyRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		metric  string
		labels  map[string]string
		wantKey string
	}{
		{
			name:    "no labels",
			metric:  "cpu_usage_percent",
			wantKey: "cpu_usage_percent",
		},
		{
			name:    "ordinary labels",
			metric:  "http_requests_total",
			labels:  map[string]string{"host": "web-1", "region": "us-east-1"},
			wantKey: "http_requests_total{host=web-1,region=us-east-1}",
		},
		{
			name:    "comma in a value",
			metric:  "cpu",
			labels:  map[string]string{"a": "b,c"},
			wantKey: `cpu{a=b\,c}`,
		},
		{
			name:    "equals in a value",
			metric:  "cpu",
			labels:  map[string]string{"path": "/query?name=cpu"},
			wantKey: `cpu{path=/query?name\=cpu}`,
		},
		{
			name:    "braces in the name",
			metric:  "cpu{a=b}",
			wantKey: `cpu\{a\=b\}`,
		},
		{
			name:    "backslash is itself escaped",
			metric:  "cpu",
			labels:  map[string]string{"win": `C:\tmp`},
			wantKey: `cpu{win=C:\\tmp}`,
		},
		{
			name:    "empty label name and value",
			metric:  "cpu",
			labels:  map[string]string{"": ""},
			wantKey: "cpu{=}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			key := SeriesKey(tt.metric, tt.labels)
			if key != tt.wantKey {
				t.Fatalf("key: want %q, got %q", tt.wantKey, key)
			}

			gotName, gotLabels, err := ParseSeriesKey(key)
			if err != nil {
				t.Fatalf("ParseSeriesKey(%q): %v", key, err)
			}
			if gotName != tt.metric {
				t.Errorf("name: want %q, got %q", tt.metric, gotName)
			}
			if len(tt.labels) == 0 && len(gotLabels) == 0 {
				return
			}
			if !maps.Equal(tt.labels, gotLabels) {
				t.Errorf("labels: want %v, got %v", tt.labels, gotLabels)
			}
		})
	}
}

func TestParseSeriesKeyRejectsMalformedInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		key     string
		wantErr string
	}{
		{"unescaped comma in the name", "cpu,x", "unescaped ','"},
		{"unescaped closing brace", "cpu}", "unescaped '}'"},
		{"label with no value", "cpu{a}", "unescaped '}'"},
		{"unterminated label section", "cpu{a=b", "unterminated label section"},
		{"trailing data after the close", "cpu{a=b}x", "trailing data"},
		{"duplicate label", `cpu{a=b,a=c}`, "duplicate label"},
		{"trailing escape", `cpu\`, "trailing escape"},
		{"escape of an ordinary byte", `cpu\x`, "invalid escape"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := ParseSeriesKey(tt.key)
			if err == nil {
				t.Fatalf("ParseSeriesKey(%q): want an error, got nil", tt.key)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

// The escape is a no-op for keys that contain no delimiter, which is what makes
// the format change invisible to data written by earlier versions.
func TestSeriesKeyLeavesCleanKeysByteIdentical(t *testing.T) {
	t.Parallel()

	clean := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"cpu_usage_percent", nil, "cpu_usage_percent"},
		{"cpu", map[string]string{"agent_id": "web-1", "tenant": "acme"}, "cpu{agent_id=web-1,tenant=acme}"},
		{"disk_free_bytes", map[string]string{"mount": "/var/lib"}, "disk_free_bytes{mount=/var/lib}"},
	}
	for _, c := range clean {
		if got := SeriesKey(c.name, c.labels); got != c.want {
			t.Errorf("SeriesKey(%q, %v) = %q, want the pre-escape form %q", c.name, c.labels, got, c.want)
		}
	}
}

// The stack scratch array must not silently corrupt keys once the label count
// crosses it.
func TestSeriesKeyBeyondInlineSortLimit(t *testing.T) {
	t.Parallel()

	labels := make(map[string]string, inlineSortMax+4)
	for i := 0; i < inlineSortMax+4; i++ {
		labels[string(rune('a'+i))] = strings.Repeat("v", i)
	}
	key := SeriesKey("cpu", labels)

	name, got, err := ParseSeriesKey(key)
	if err != nil {
		t.Fatalf("ParseSeriesKey(%q): %v", key, err)
	}
	if name != "cpu" {
		t.Errorf("name: want cpu, got %q", name)
	}
	if !maps.Equal(labels, got) {
		t.Errorf("labels: want %v, got %v", labels, got)
	}
}

// Escaping must not allocate for the clean path — this is on the write path of
// every metric the system ingests.
func TestSeriesKeyDoesNotAllocateForUnlabelledCleanNames(t *testing.T) {
	allocs := testing.AllocsPerRun(100, func() {
		sinkString = SeriesKey("cpu_usage_percent", nil)
	})
	if allocs != 0 {
		t.Errorf("want 0 allocations for an unlabelled clean name, got %v", allocs)
	}
}

func TestSeriesKeyAllocatesOnceForLabelledCleanNames(t *testing.T) {
	labels := map[string]string{"host": "web-1", "region": "us-east-1", "service": "api"}
	allocs := testing.AllocsPerRun(100, func() {
		sinkString = SeriesKey("http_requests_total", labels)
	})
	if allocs != 1 {
		t.Errorf("want exactly 1 allocation (the result string), got %v", allocs)
	}
}
