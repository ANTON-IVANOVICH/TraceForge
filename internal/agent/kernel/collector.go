// Package kernel collects Linux kernel network counters — TCP retransmissions
// and resets, listen-queue overflows, UDP receive errors — straight out of
// procfs, with no CGo.
//
// # Why this package exists
//
// It is the deliberate counterpart to internal/agent/network. That package
// crosses into C (libpcap) because there is no pure-Go way to pull packets off a
// live interface. This package gets data of the same family — the numbers people
// install eBPF or link a libbpf CGo binding to obtain — and needs none of it,
// because on Linux the kernel already prints these counters as text:
//
//	/proc/net/snmp     Tcp:/Udp: SNMP MIB counters
//	/proc/net/netstat  TcpExt:/IpExt: the extended set
//
// Reading two text files is bufio + strings + strconv. No C toolchain, no
// libbpf headers, a static binary, and `GOOS=linux GOARCH=arm64 go build` still
// cross-compiles from a laptop. That is the whole lesson of the stage: before
// reaching for CGo, check the alternatives — a pure-Go reimplementation, procfs,
// a direct syscall, a subprocess, WASM. CGo is the last of them, not the first.
//
// When procfs does not expose a number, the next CGo-free step up is a raw
// syscall through golang.org/x/sys/unix (getsockopt, a netlink socket) — still
// no C, still a static cross-compile — not a C binding. This collector needs
// neither; the files are enough.
//
// # The split
//
// parse.go is platform-independent on purpose: it takes an io.Reader, so its
// tests run on the macOS laptop where /proc does not exist. The file-reading
// wiring — the /proc paths and the availability check — lives behind
// //go:build linux in collector_linux.go, with a stub in collector_other.go, the
// same shape as the network collector's pcap_cgo.go / pcap_nocgo.go split.
package kernel

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"metrics-system/internal/model"
)

// ErrUnavailable reports that kernel network counters cannot be read here: the
// host is not Linux, or /proc/net/snmp is absent. The agent treats it exactly as
// it treats the network collector's ErrUnsupported — the collector is left out,
// not fatal. An agent that refuses to start because one source is missing is
// worse than one that starts with the sources it has.
var ErrUnavailable = errors.New("kernel metrics unavailable: /proc/net/snmp not present (non-Linux host?)")

// Collector reads kernel network counters from procfs on every Collect.
//
// The proc paths are fields, not constants baked into Collect, so a test can aim
// the file-reading path at a fixture. Production sets them once, in the Linux
// NewCollector, and never changes them — there is no runtime knob.
type Collector struct {
	hostname    string
	snmpPath    string
	netstatPath string
}

// Name identifies the collector in the agent's logs.
func (c *Collector) Name() string { return "kernel" }

// Collect reads and parses both files on every call. Re-reading each tick is the
// point, not an oversight: these are the kernel's live counters and each file is
// a few hundred bytes, so there is nothing to cache and nothing a stale copy
// would buy. The context is unused because a procfs read does not block on
// anything an agent-scale timeout would rescue it from.
func (c *Collector) Collect(_ context.Context) ([]model.Metric, error) {
	snmp, err := os.Open(c.snmpPath)
	if err != nil {
		// Present when the collector was built, gone now: surface it so a
		// vanished procfs shows up in the logs instead of a silent gap in the
		// metrics.
		return nil, err
	}
	defer func() { _ = snmp.Close() }()

	// netstat is best-effort. Its absence costs two metrics (listen-queue
	// overflows and drops), not the whole collection, so a failure to open it is
	// swallowed rather than propagated.
	var netstat io.Reader
	if f, err := os.Open(c.netstatPath); err == nil {
		defer func() { _ = f.Close() }()
		netstat = f
	}

	return collect(snmp, netstat, c.hostname, time.Now().UTC())
}

// collect is the reader-level core Collect wraps and the tests drive directly.
// Separating it from os.Open is what lets the whole pipeline — parse, merge,
// curate — run against a strings.Reader on a platform with no /proc.
func collect(snmp, netstat io.Reader, hostname string, now time.Time) ([]model.Metric, error) {
	stats, err := parseProcNet(snmp)

	// TcpExt lives only in netstat, Tcp/Udp only in snmp, so the two prefix sets
	// never collide and a plain merge is enough. A netstat parse error drops its
	// two fields rather than the snmp ones already in hand.
	if netstat != nil {
		if ext, extErr := parseProcNet(netstat); extErr == nil {
			for prefix, fields := range ext {
				stats[prefix] = fields
			}
		}
	}

	// Metrics are still built from whatever parsed even when err != nil, so the
	// fuzz target can assert on the output of a partial parse. The real Collect
	// path never sees a scanner error against a genuine procfs file.
	return buildMetrics(stats, hostname, now), err
}

// metricSpec maps one emitted metric onto the (prefix, field) that feeds it.
type metricSpec struct {
	name   string
	typ    model.MetricType
	prefix string
	field  string
}

// The emitted set is deliberately tiny. The two files carry well over a hundred
// fields between them; exporting all of them would bolt a hundred flat-zero
// series onto every host in the fleet and bury the four or five numbers anyone
// actually pages on. Exploding into 60 series per host is the cardinality
// mistake this project is built to avoid. These are the numbers that turn "the
// network feels slow" into a cause:
//
//   - RetransSegs — TCP segments retransmitted, the first quantitative sign of
//     loss or congestion.
//   - OutRsts — resets this host sent, which climb when a service is refusing or
//     tearing down connections.
//   - CurrEstab — connections established right now. A gauge, not a counter: it
//     rises and falls, so rate() over it is nonsense (see model.MetricTypeGauge).
//   - ListenOverflows / ListenDrops — the accept queue overflowed and new
//     connections were dropped because the application is not accept()ing fast
//     enough. The classic "why do requests time out under load" pair, and a
//     reason people reach for eBPF that procfs answers for free.
//   - Udp InErrors / NoPorts — UDP datagrams that failed at receive, and
//     datagrams for a port nothing was listening on: the UDP echoes of the TCP
//     failure signals above.
//
// Everything else — per-ICMP-type counts, IP fragmentation stats, TCP fast-open
// internals — stays in the file for a human with `cat`, not shipped as a series.
var metricSpecs = []metricSpec{
	{"tcp_retransmit_segments_total", model.MetricTypeCounter, "Tcp", "RetransSegs"},
	{"tcp_connection_resets_total", model.MetricTypeCounter, "Tcp", "OutRsts"},
	{"tcp_established_connections", model.MetricTypeGauge, "Tcp", "CurrEstab"},
	{"tcp_listen_overflows_total", model.MetricTypeCounter, "TcpExt", "ListenOverflows"},
	{"tcp_listen_drops_total", model.MetricTypeCounter, "TcpExt", "ListenDrops"},
	{"udp_receive_errors_total", model.MetricTypeCounter, "Udp", "InErrors"},
	{"udp_no_ports_total", model.MetricTypeCounter, "Udp", "NoPorts"},
}

// buildMetrics turns the parsed counters into the curated metric set. A field
// that is absent or negative is skipped, never emitted: a metric that lies is
// worse than a metric that is missing, and both this file's counters and the one
// gauge it exports are quantities that cannot legitimately be below zero.
func buildMetrics(stats procStats, hostname string, now time.Time) []model.Metric {
	// One shared label map, read-only after this point, matching the other
	// collectors (memory.go does the same). host is the only label — these are
	// host-global kernel counters, and there is no second dimension to split them
	// on without inventing cardinality.
	labels := map[string]string{"host": hostname}

	out := make([]model.Metric, 0, len(metricSpecs))
	for _, s := range metricSpecs {
		v, ok := stats.get(s.prefix, s.field)
		if !ok {
			continue // field not emitted by this kernel: skip, do not invent a 0
		}
		if v < 0 {
			// A negative counter is a wrapped or bugged value; a negative
			// CurrEstab is impossible, since a connection count cannot go below
			// zero. Either way the number is a lie. This check is per-metric, not
			// in the parser, precisely because a field like MaxConn where -1 is
			// meaningful would deserve to pass — none of the chosen set is such a
			// field, so all of them are dropped when negative.
			continue
		}
		out = append(out, model.Metric{
			Name:      s.name,
			Type:      s.typ,
			Value:     float64(v),
			Timestamp: now,
			Labels:    labels,
		})
	}
	return out
}
