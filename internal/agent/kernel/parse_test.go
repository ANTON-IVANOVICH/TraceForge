package kernel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/testutil"
)

// A normal /proc/net/snmp, trimmed to the sections the collector reads plus a
// couple it ignores, with MaxConn at its real -1 and the field order the kernel
// actually prints.
const normalSNMP = `Ip: Forwarding DefaultTTL InReceives InHdrErrors InAddrErrors ForwDatagrams InUnknownProtos InDiscards InDelivers OutRequests OutDiscards OutNoRoutes ReasmTimeout ReasmReqds ReasmOKs ReasmFails FragOKs FragFails FragCreates
Ip: 1 64 1234567 0 0 0 0 0 1234000 1200000 12 34 0 0 0 0 0 0 0
Tcp: RtoAlgorithm RtoMin RtoMax MaxConn ActiveOpens PassiveOpens AttemptFails EstabResets CurrEstab InSegs OutSegs RetransSegs InErrs OutRsts InCsumErrors
Tcp: 1 200 120000 -1 54321 12345 100 50 42 9876543 8765432 1234 0 567 0
Udp: InDatagrams NoPorts InErrors OutDatagrams RcvbufErrors SndbufErrors InCsumErrors IgnoredMulti MemErrors
Udp: 456789 321 12 445566 0 0 0 5 0
UdpLite: InDatagrams NoPorts InErrors OutDatagrams RcvbufErrors SndbufErrors InCsumErrors IgnoredMulti MemErrors
UdpLite: 0 0 0 0 0 0 0 0 0
`

// A normal /proc/net/netstat, again trimmed but keeping ListenOverflows and
// ListenDrops in a realistic position amid neighbours the collector skips.
const normalNetstat = `TcpExt: SyncookiesSent SyncookiesRecv DelayedACKs ListenOverflows ListenDrops TCPHPHits TCPPureAcks TCPSynRetrans
TcpExt: 0 0 200 4 6 9000 3000 17
IpExt: InNoRoutes InTruncatedPkts InMcastPkts InOctets OutOctets
IpExt: 0 0 10 123456789 98765432
`

func TestParseProcNet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		check func(t *testing.T, s procStats)
	}{
		{
			name:  "normal snmp parses by name",
			input: normalSNMP,
			check: func(t *testing.T, s procStats) {
				wantPresent(t, s, "Tcp", "RetransSegs", 1234)
				wantPresent(t, s, "Tcp", "OutRsts", 567)
				wantPresent(t, s, "Tcp", "CurrEstab", 42)
				wantPresent(t, s, "Udp", "InErrors", 12)
				wantPresent(t, s, "Udp", "NoPorts", 321)
			},
		},
		{
			name:  "normal netstat parses TcpExt",
			input: normalNetstat,
			check: func(t *testing.T, s procStats) {
				wantPresent(t, s, "TcpExt", "ListenOverflows", 4)
				wantPresent(t, s, "TcpExt", "ListenDrops", 6)
			},
		},
		{
			// The whole reason to parse by name: a kernel that lists the same
			// fields in a different order must yield the same numbers. Columns
			// here are deliberately scrambled relative to normalSNMP.
			name: "different field order",
			input: `Tcp: OutRsts CurrEstab MaxConn RetransSegs
Tcp: 99 7 -1 88
`,
			check: func(t *testing.T, s procStats) {
				wantPresent(t, s, "Tcp", "RetransSegs", 88)
				wantPresent(t, s, "Tcp", "OutRsts", 99)
				wantPresent(t, s, "Tcp", "CurrEstab", 7)
			},
		},
		{
			// A kernel that simply does not emit RetransSegs. The field is absent,
			// which must read as absent — not as a fabricated zero.
			name: "field missing from header",
			input: `Tcp: RtoAlgorithm CurrEstab OutRsts
Tcp: 1 5 3
`,
			check: func(t *testing.T, s procStats) {
				wantAbsent(t, s, "Tcp", "RetransSegs")
				wantPresent(t, s, "Tcp", "CurrEstab", 5)
				wantPresent(t, s, "Tcp", "OutRsts", 3)
			},
		},
		{
			name: "negative value is preserved as signed",
			input: `Tcp: MaxConn CurrEstab
Tcp: -1 3
`,
			check: func(t *testing.T, s procStats) {
				wantPresent(t, s, "Tcp", "MaxConn", -1)
				wantPresent(t, s, "Tcp", "CurrEstab", 3)
			},
		},
		{
			// A header with no value line after it (the file was cut off). The
			// dangling section contributes nothing; the complete one before it
			// survives.
			name: "truncated after a header line",
			input: `Udp: InDatagrams NoPorts
Udp: 5 6
Tcp: RetransSegs OutRsts
`,
			check: func(t *testing.T, s procStats) {
				wantPresent(t, s, "Udp", "NoPorts", 6)
				wantAbsent(t, s, "Tcp", "RetransSegs")
				wantAbsent(t, s, "Tcp", "OutRsts")
			},
		},
		{
			// Header names three columns, value line gives two. There is no way to
			// tell which column was lost, so the entire pairing is dropped rather
			// than misaligned.
			name: "mismatched header and value lengths",
			input: `Tcp: RetransSegs OutRsts CurrEstab
Tcp: 10 20
`,
			check: func(t *testing.T, s procStats) {
				wantAbsent(t, s, "Tcp", "RetransSegs")
				wantAbsent(t, s, "Tcp", "OutRsts")
				wantAbsent(t, s, "Tcp", "CurrEstab")
			},
		},
		{
			name:  "empty file",
			input: "",
			check: func(t *testing.T, s procStats) {
				wantAbsent(t, s, "Tcp", "RetransSegs")
				if len(s) != 0 {
					t.Errorf("empty input produced %d sections, want 0", len(s))
				}
			},
		},
		{
			// A section that appears twice: last pairing wins, as it does when the
			// kernel re-emits a block.
			name: "repeated prefix, last wins",
			input: `Tcp: RetransSegs
Tcp: 10
Tcp: RetransSegs
Tcp: 20
`,
			check: func(t *testing.T, s procStats) {
				wantPresent(t, s, "Tcp", "RetransSegs", 20)
			},
		},
		{
			// A single non-numeric column must not poison its numeric siblings on
			// the same line.
			name: "non-numeric value column is skipped, siblings survive",
			input: `Tcp: RetransSegs OutRsts CurrEstab
Tcp: 10 garbage 30
`,
			check: func(t *testing.T, s procStats) {
				wantPresent(t, s, "Tcp", "RetransSegs", 10)
				wantAbsent(t, s, "Tcp", "OutRsts")
				wantPresent(t, s, "Tcp", "CurrEstab", 30)
			},
		},
		{
			name:  "blank lines and extra whitespace are ignored",
			input: "\n   \nTcp:   RetransSegs    OutRsts\nTcp:  7   8 \n\n",
			check: func(t *testing.T, s procStats) {
				wantPresent(t, s, "Tcp", "RetransSegs", 7)
				wantPresent(t, s, "Tcp", "OutRsts", 8)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := parseProcNet(strings.NewReader(tt.input))
			if err != nil {
				t.Fatalf("parseProcNet returned error: %v", err)
			}
			tt.check(t, s)
		})
	}
}

func TestBuildMetricsCuratedSet(t *testing.T) {
	t.Parallel()

	now := testutil.BaseTime
	metrics, err := collect(strings.NewReader(normalSNMP), strings.NewReader(normalNetstat), "host-a", now)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	// Exactly the curated set, no more: seven fields are present and non-negative
	// in the fixtures, so all seven appear and nothing else does.
	want := []model.Metric{
		metric("tcp_retransmit_segments_total", model.MetricTypeCounter, 1234, now),
		metric("tcp_connection_resets_total", model.MetricTypeCounter, 567, now),
		metric("tcp_established_connections", model.MetricTypeGauge, 42, now),
		metric("tcp_listen_overflows_total", model.MetricTypeCounter, 4, now),
		metric("tcp_listen_drops_total", model.MetricTypeCounter, 6, now),
		metric("udp_receive_errors_total", model.MetricTypeCounter, 12, now),
		metric("udp_no_ports_total", model.MetricTypeCounter, 321, now),
	}
	testutil.AssertMetricsEqual(t, want, metrics)

	// Every emitted metric must survive the model's own validation gate.
	for _, m := range metrics {
		if err := m.Validate(); err != nil {
			t.Errorf("metric %q does not validate: %v", m.Name, err)
		}
	}
}

// CurrEstab is the one gauge in the set. Emitting it as a counter would invite
// rate() over a value that falls as often as it rises — a meaningless series.
func TestEstablishedConnectionsIsAGauge(t *testing.T) {
	t.Parallel()

	metrics, err := collect(strings.NewReader(normalSNMP), strings.NewReader(normalNetstat), "h", testutil.BaseTime)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := findMetric(metrics, "tcp_established_connections")
	if !ok {
		t.Fatal("tcp_established_connections not emitted")
	}
	if m.Type != model.MetricTypeGauge {
		t.Errorf("tcp_established_connections type = %s, want gauge", m.Type)
	}
	for _, other := range metrics {
		if other.Name != "tcp_established_connections" && other.Type != model.MetricTypeCounter {
			t.Errorf("%s type = %s, want counter", other.Name, other.Type)
		}
	}
}

// A negative counter and an absent field are both dropped, never emitted.
func TestNegativeAndMissingAreSkipped(t *testing.T) {
	t.Parallel()

	// RetransSegs negative (must be dropped), OutRsts absent (must be dropped),
	// CurrEstab positive (must survive).
	snmp := `Tcp: RetransSegs CurrEstab
Tcp: -5 3
`
	metrics, err := collect(strings.NewReader(snmp), nil, "h", testutil.BaseTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findMetric(metrics, "tcp_retransmit_segments_total"); ok {
		t.Error("negative RetransSegs was emitted; it must be skipped")
	}
	if _, ok := findMetric(metrics, "tcp_connection_resets_total"); ok {
		t.Error("absent OutRsts was emitted; it must be skipped")
	}
	if m, ok := findMetric(metrics, "tcp_established_connections"); !ok || m.Value != 3 {
		t.Errorf("CurrEstab: got %+v, ok=%v; want value 3", m, ok)
	}
}

// A nil netstat reader (its file was missing) must cost only the two TcpExt
// metrics, not the whole collection.
func TestNetstatOptional(t *testing.T) {
	t.Parallel()

	metrics, err := collect(strings.NewReader(normalSNMP), nil, "h", testutil.BaseTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findMetric(metrics, "tcp_listen_overflows_total"); ok {
		t.Error("listen overflows emitted without a netstat source")
	}
	if _, ok := findMetric(metrics, "tcp_retransmit_segments_total"); !ok {
		t.Error("snmp metrics must still be emitted when netstat is absent")
	}
}

// The file-reading path (os.Open, defer Close, netstat-optional) exercised
// end-to-end against real files, on whatever OS the test runs — the Collector is
// built directly here rather than through the Linux-only NewCollector.
func TestCollectFromFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	snmpPath := filepath.Join(dir, "snmp")
	netstatPath := filepath.Join(dir, "netstat")
	if err := os.WriteFile(snmpPath, []byte(normalSNMP), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(netstatPath, []byte(normalNetstat), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Collector{hostname: "host-b", snmpPath: snmpPath, netstatPath: netstatPath}
	if c.Name() != "kernel" {
		t.Errorf("Name = %q, want kernel", c.Name())
	}

	metrics, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if _, ok := findMetric(metrics, "tcp_listen_overflows_total"); !ok {
		t.Error("expected TcpExt metric from the netstat file")
	}

	// A missing snmp file is a hard error; a missing netstat file is not.
	if err := os.Remove(netstatPath); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Collect(context.Background()); err != nil {
		t.Errorf("Collect with netstat gone should still succeed: %v", err)
	}
	if err := os.Remove(snmpPath); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Collect(context.Background()); err == nil {
		t.Error("Collect with snmp gone should return an error")
	}
}

// NewCollector is platform-specific: it succeeds on a real Linux procfs and
// returns ErrUnavailable everywhere else. This asserts the contract on whatever
// host runs it, so it passes both on the macOS laptop and in Linux CI.
func TestNewCollectorContract(t *testing.T) {
	t.Parallel()

	c, err := NewCollector("host-c")
	if err != nil {
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("want ErrUnavailable, got %v", err)
		}
		return
	}
	if c.Name() != "kernel" {
		t.Errorf("Name = %q, want kernel", c.Name())
	}
	if _, err := c.Collect(context.Background()); err != nil {
		t.Errorf("Collect against a real procfs: %v", err)
	}
}

func wantPresent(t *testing.T, s procStats, prefix, field string, want int64) {
	t.Helper()
	got, ok := s.get(prefix, field)
	if !ok {
		t.Errorf("%s.%s: absent, want %d", prefix, field, want)
		return
	}
	if got != want {
		t.Errorf("%s.%s = %d, want %d", prefix, field, got, want)
	}
}

func wantAbsent(t *testing.T, s procStats, prefix, field string) {
	t.Helper()
	if got, ok := s.get(prefix, field); ok {
		t.Errorf("%s.%s = %d, want absent", prefix, field, got)
	}
}

func metric(name string, typ model.MetricType, v float64, now time.Time) model.Metric {
	return model.Metric{
		Name:      name,
		Type:      typ,
		Value:     v,
		Timestamp: now,
		Labels:    map[string]string{"host": "host-a"},
	}
}

func findMetric(metrics []model.Metric, name string) (model.Metric, bool) {
	for _, m := range metrics {
		if m.Name == name {
			return m, true
		}
	}
	return model.Metric{}, false
}

// A line must name a protocol. Found by FuzzParseProcNet, which fed the parser
// ":\n:" and got back a section whose prefix was the empty string.
func TestParseRejectsLinesThatNameNoProtocol(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"a bare colon", ":\n:"},
		{"a colon with columns", ": A B\n: 1 2"},
		{"no colon at all", "Tcp A B\nTcp 1 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stats, err := parseProcNet(strings.NewReader(tt.in))
			if err != nil {
				t.Fatalf("parseProcNet: %v", err)
			}
			if len(stats) != 0 {
				t.Errorf("want no sections, got %v", stats)
			}
		})
	}
}
