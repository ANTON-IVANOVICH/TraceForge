package grpcconv

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"metrics-system/internal/model"
	metricspb "metrics-system/internal/proto/metricspb"
	"metrics-system/internal/testutil"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// protoSecMin/Max bound a timestamp to protobuf's documented valid range
// (0001-01-01 .. 9999-12-31). Inside it, timestamppb.New/AsTime round-trips a
// (seconds, nanos) pair exactly; outside it the time package's own internal
// overflow — not our converter — would break the trip, which we must not blame
// on the code under test.
const (
	protoSecMin = -62135596800
	protoSecMax = 253402300799
)

func clampUnix(sec int64) int64 {
	switch {
	case sec < protoSecMin:
		return protoSecMin
	case sec > protoSecMax:
		return protoSecMax
	default:
		return sec
	}
}

// FuzzProtoRoundTrip asserts MetricToProto→MetricFromProto is lossless for every
// field. Value is compared by raw bits (math.Float64bits), which deliberately
// treats NaN as equal to itself: the point is that protobuf carries NaN and Inf
// faithfully — the exact values encoding/json refuses — so the gRPC boundary,
// not this converter, is where a non-finite value must be stopped (see
// TestNonFiniteSmuggledThroughProto). Timestamps are compared with time.Equal
// because AsTime always returns UTC.
func FuzzProtoRoundTrip(f *testing.F) {
	f.Add("cpu", false, 42.5, int64(1_700_000_000), int64(123), "host", "web-1")
	f.Add("reqs", true, 0.0, int64(0), int64(0), "", "")
	f.Add("g", false, math.Inf(1), int64(-1), int64(999_999_999), "region", "eu")
	f.Add("n", true, math.NaN(), int64(protoSecMax), int64(0), "k", "v")

	f.Fuzz(func(t *testing.T, name string, counter bool, value float64, sec, nano int64, lk, lv string) {
		mt := model.MetricTypeGauge
		if counter {
			mt = model.MetricTypeCounter
		}
		ts := time.Unix(clampUnix(sec), ((nano%1e9)+1e9)%1e9).UTC()
		m := model.Metric{Name: name, Type: mt, Value: value, Timestamp: ts}
		if lk != "" {
			m.Labels = map[string]string{lk: lv}
		}

		back, err := MetricFromProto(MetricToProto(m))
		if err != nil {
			// Only gauge/counter are built, both of which are valid proto enums.
			t.Fatalf("valid metric failed conversion: %v", err)
		}
		if back.Name != m.Name {
			t.Fatalf("name: got %q want %q", back.Name, m.Name)
		}
		if back.Type != m.Type {
			t.Fatalf("type: got %v want %v", back.Type, m.Type)
		}
		if math.Float64bits(back.Value) != math.Float64bits(m.Value) {
			t.Fatalf("value bits: got %x want %x", math.Float64bits(back.Value), math.Float64bits(m.Value))
		}
		if !back.Timestamp.Equal(m.Timestamp) {
			t.Fatalf("timestamp: got %v want %v", back.Timestamp, m.Timestamp)
		}
		if len(back.Labels) != len(m.Labels) {
			t.Fatalf("label count: got %d want %d", len(back.Labels), len(m.Labels))
		}
		for k, v := range m.Labels {
			if back.Labels[k] != v {
				t.Fatalf("label %q: got %q want %q", k, back.Labels[k], v)
			}
		}
	})
}

// FuzzBatchFromProtoBytes decodes arbitrary bytes as a protobuf Batch and
// converts it. proto.Unmarshal and the conversion must never panic on malformed
// or hostile wire data; a decode or convert error is a fine outcome.
func FuzzBatchFromProtoBytes(f *testing.F) {
	seed := BatchToProto(testutil.NewBatch().
		WithMetrics(testutil.NewMetric().WithLabel("host", "web-1").Build()).
		Build())
	if raw, err := proto.Marshal(seed); err == nil {
		f.Add(raw)
	}
	f.Add([]byte(nil))
	f.Add([]byte{0x08, 0x96, 0x01})       // a stray varint field
	f.Add([]byte{0xff, 0xff, 0xff, 0xff}) // truncated garbage

	f.Fuzz(func(t *testing.T, data []byte) {
		var pb metricspb.Batch
		if err := proto.Unmarshal(data, &pb); err != nil {
			return
		}
		b, err := BatchFromProto(&pb)
		if err != nil {
			return
		}
		_ = b.Validate() // must not panic on decoded-but-invalid batches
	})
}

// TestNonFiniteSmuggledThroughProto documents and guards the transport
// asymmetry: NaN/Inf cannot cross the HTTP/JSON boundary (encoding/json refuses
// them), yet protobuf carries them and MetricFromProto — a pure converter — does
// not reject them. The shared model.Validate gate, which both transports run
// before storing, is what closes the hole.
func TestNonFiniteSmuggledThroughProto(t *testing.T) {
	t.Parallel()
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		// HTTP path: this value has no JSON representation at all.
		if _, err := json.Marshal(v); err == nil {
			t.Fatalf("json.Marshal(%v) unexpectedly succeeded", v)
		}
		// gRPC path: proto conveys it and the converter passes it through untouched.
		m, err := MetricFromProto(&metricspb.Metric{
			Name:      "cpu",
			Type:      metricspb.MetricType_METRIC_TYPE_GAUGE,
			Value:     v,
			Timestamp: timestamppb.New(time.Unix(1, 0)),
		})
		if err != nil {
			t.Fatalf("MetricFromProto(%v): unexpected error %v", v, err)
		}
		// The choke point both transports share must reject it.
		if err := m.Validate(); err == nil {
			t.Fatalf("Validate accepted non-finite value %v smuggled over protobuf", v)
		}
	}
}
