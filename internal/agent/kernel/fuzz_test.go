package kernel

import (
	"maps"
	"math"
	"strconv"
	"strings"
	"testing"

	"metrics-system/internal/model"
	"metrics-system/internal/testutil"
)

// The parser reads a file the kernel writes, but a fuzzer is a better adversary
// than the kernel: it sends the truncations, the ragged columns, the numbers
// that do not fit an int64, and the megabyte line. The invariant is total —
// whatever the bytes, collect returns without panicking, and every metric it
// hands back is one the rest of the system will accept:
//
//   - finite value (guaranteed anyway, since int64→float64 cannot produce
//     NaN/Inf, but asserted so a future change to the value path cannot break it
//     unnoticed),
//   - non-negative when it is a counter,
//   - and valid by model.Metric.Validate, the same gate the transports enforce.
//
// A metric that fails any of these is a metric the collector should have dropped
// at the source rather than shipped as a confidently wrong number.
func FuzzCollect(f *testing.F) {
	f.Add(normalSNMP, normalNetstat)
	f.Add(normalSNMP, "")
	f.Add("", "")
	f.Add("Tcp:\nTcp:\n", "")
	f.Add("Tcp: RetransSegs OutRsts\nTcp: -1 -9999999999999999999999\n", "TcpExt: ListenDrops\nTcpExt: -5\n")
	f.Add("Tcp: A B C\nTcp: 1 2\n", "TcpExt: ListenOverflows\nTcpExt: notanumber\n")
	f.Add("Udp: NoPorts InErrors\nUdp: 4 8\n", "IpExt: InOctets\nIpExt: 1\n")

	f.Fuzz(func(t *testing.T, snmp, netstat string) {
		metrics, _ := collect(strings.NewReader(snmp), strings.NewReader(netstat), "host", testutil.BaseTime)

		for _, m := range metrics {
			if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
				t.Fatalf("non-finite value for %s: %v", m.Name, m.Value)
			}
			if m.Type == model.MetricTypeCounter && m.Value < 0 {
				t.Fatalf("negative counter %s: %v", m.Name, m.Value)
			}
			if err := m.Validate(); err != nil {
				t.Fatalf("emitted metric %q does not validate: %v", m.Name, err)
			}
		}
	})
}

// The parser alone must never panic, and — the invariant worth having — must
// never *invent* data.
//
// Asserting that an int64 maps to a finite float64 would be asserting a fact
// about IEEE 754, not about this parser: the check cannot fail, whatever the
// code does. What can fail is a parser that mis-associates a column with the
// wrong header, or that carries a value over from a previous section. So the
// invariant is provenance: every field name and every value it reports must
// literally appear in the bytes it was given.
//
// It is not a proof of correct pairing — "5" is a substring of "15" — but it is
// enough to catch a parser that fabricates, and it is checked on inputs no
// hand-written table would think to try.
func FuzzParseProcNet(f *testing.F) {
	f.Add(normalSNMP)
	f.Add(normalNetstat)
	f.Add("Tcp: RetransSegs\nTcp: 99999999999999999999999999\n")
	f.Add("Tcp: A B\nTcp: 1 2\nTcp: C\nTcp: 3\n")
	f.Add(strings.Repeat("Tcp: A\n", 1000))

	f.Fuzz(func(t *testing.T, data string) {
		stats, _ := parseProcNet(strings.NewReader(data))

		for prefix, fields := range stats {
			if prefix == "" {
				t.Fatalf("parser returned an empty prefix for input %q", data)
			}
			if !strings.Contains(data, prefix) {
				t.Fatalf("prefix %q was never in the input %q", prefix, data)
			}
			for name, v := range fields {
				if name == "" {
					t.Fatalf("%s: parser returned an empty field name", prefix)
				}
				if !strings.Contains(data, name) {
					t.Fatalf("%s.%s: field name was never in the input %q", prefix, name, data)
				}
				if rendered := strconv.FormatInt(v, 10); !strings.Contains(data, rendered) {
					t.Fatalf("%s.%s = %d: that value was never in the input %q", prefix, name, v, data)
				}
			}
		}

		// Parsing is a pure function of the bytes: two reads of the same input
		// must agree, or some state is leaking between calls.
		again, _ := parseProcNet(strings.NewReader(data))
		if !maps.EqualFunc(stats, again, func(a, b map[string]int64) bool { return maps.Equal(a, b) }) {
			t.Fatalf("parseProcNet is not deterministic for %q", data)
		}
	})
}
