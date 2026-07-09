package model_test

import (
	"encoding/json"
	"math"
	"testing"

	"metrics-system/internal/model"
	"metrics-system/internal/testutil"
)

// FuzzMetricJSONRoundTrip asserts the JSON codec's central invariant: any byte
// string that decodes into a Metric must re-encode without error and decode
// again to an equal value. The asymmetry worth hunting is MetricType's custom
// codec — it accepts both a string ("gauge") and a number (0/1) on the way in
// but only ever emits a string on the way out. If some value could unmarshal yet
// not marshal, the round-trip breaks here rather than in a production response.
func FuzzMetricJSONRoundTrip(f *testing.F) {
	seeds := []string{
		`{"name":"cpu","type":"gauge","value":1.5,"timestamp":"2026-01-01T00:00:00Z"}`,
		`{"name":"reqs","type":"counter","value":42,"timestamp":"2026-01-01T00:00:00Z","labels":{"host":"web-1"}}`,
		`{"name":"n","type":0,"value":0,"timestamp":"2026-01-01T00:00:00Z"}`,              // numeric type in
		`{"name":"n","type":1,"value":-1e9,"timestamp":"1970-01-01T00:00:00.5Z"}`,         // numeric type, fractional second
		`{"name":"n","type":"COUNTER","value":3,"timestamp":"2026-01-01T00:00:00+05:00"}`, // mixed case + zone offset
		`{"type":"gauge"}`, // missing scalar fields
		`{}`,               // empty object: type defaults to gauge(0)
		`not json`,
		``,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var first model.Metric
		if err := json.Unmarshal(data, &first); err != nil {
			return // not a Metric; nothing to round-trip
		}
		// A value that decoded must re-encode: if this fails, an accepted input has
		// no canonical wire form — exactly the string-or-number MetricType hazard.
		out, err := json.Marshal(first)
		if err != nil {
			t.Fatalf("re-marshal of a decoded Metric failed: %v (from %q)", err, data)
		}
		var second model.Metric
		if err := json.Unmarshal(out, &second); err != nil {
			t.Fatalf("re-unmarshal of self-produced JSON failed: %v (%q)", err, out)
		}
		// Canonical output makes the second decode a fixed point of the first.
		testutil.AssertMetricEqual(t, first, second)
		// encoding/json cannot decode NaN/Inf, so Value must be finite; assert it so
		// a future codec change that lets one through is caught at the boundary.
		if math.IsNaN(second.Value) || math.IsInf(second.Value, 0) {
			t.Fatalf("non-finite value survived JSON decode: %v", second.Value)
		}
	})
}

// FuzzBatchValidate feeds hostile JSON into a Batch and validates it. The
// invariant is robustness: neither the decode nor Validate may panic, whatever
// shape the metrics slice, labels map or scalar fields take.
func FuzzBatchValidate(f *testing.F) {
	seeds := []string{
		`{"agent_id":"a","metrics":[{"name":"n","type":"gauge","value":1,"timestamp":"2026-01-01T00:00:00Z"}]}`,
		`{"agent_id":"","metrics":[]}`,
		`{"metrics":[{"name":"","type":"gauge","timestamp":"2026-01-01T00:00:00Z"}]}`,
		`{"agent_id":"a"}`,
		`{"agent_id":"a","metrics":null}`,
		`garbage`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var b model.Batch
		if err := json.Unmarshal(data, &b); err != nil {
			return
		}
		_ = b.Validate() // must not panic; the verdict itself is irrelevant here
	})
}
