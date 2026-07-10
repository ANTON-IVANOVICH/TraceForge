// Package network collects network metrics by capturing packets through
// libpcap — the C library behind tcpdump and Wireshark. It is the project's one
// crossing into C, and it exists because there is no way to read packets off a
// live interface from pure Go without reimplementing BPF on every platform.
//
// # The CGo boundary
//
// Everything in this package obeys one rule: Go's garbage collector knows
// nothing about C memory, and C knows nothing about the collector. Every
// violation is undefined behaviour, and undefined behaviour in a monitoring
// agent means the thing watching your fleet is the thing that crashes it.
//
// Concretely:
//
//   - A C string built with C.CString lives in the C heap. Go will never free
//     it; every one is paired with a C.free.
//   - libpcap hands back a pointer into its own reusable receive buffer. The
//     bytes are valid only until the next call. Every packet is copied into the
//     Go heap with C.GoBytes before it is returned.
//   - A *C.pcap_t is invisible to the GC. Its lifetime is ours: Close releases
//     it, and a runtime.AddCleanup is the safety net for a caller who forgot.
//   - A pcap_t is not thread-safe. Close must not free a handle another
//     goroutine is inside. A mutex serialises every use of it.
//
// # Why this file has no CGo
//
// The types below are shared by the CGo implementation and by the stub that
// replaces it when the binary is built with CGO_ENABLED=0. Keeping them here,
// free of `import "C"`, is what lets `go build` succeed on a machine with no C
// compiler and no libpcap — the agent still builds, and the network collector
// reports that it is unavailable rather than failing the build.
package network

import (
	"errors"
	"time"
)

// ErrUnsupported is returned when packet capture is not available in this
// binary: it was built without CGo, or on a platform with no libpcap.
var ErrUnsupported = errors.New("network capture unavailable: this binary was built without CGo (see -network)")

// ErrCaptureClosed is returned by any operation on a closed Capture.
var ErrCaptureClosed = errors.New("capture is closed")

// ErrEndOfCapture is returned by Next when an offline capture file is
// exhausted. A live capture never returns it.
var ErrEndOfCapture = errors.New("end of capture")

// ErrTimeout is returned by Next when a live capture produced no packet within
// the configured timeout. It is not a failure — it is the read loop's chance to
// notice that its context was cancelled.
//
// It lives here, not in the CGo file, because collector.go handles it and
// collector.go must compile in the CGO_ENABLED=0 build too. Discovering that is
// exactly what the no-CGo build is for.
var ErrTimeout = errors.New("capture timeout")

// Packet is one captured frame, already copied into the Go heap.
type Packet struct {
	// Timestamp is when the kernel saw the packet, not when Go read it.
	Timestamp time.Time
	// WireLength is the packet's length on the wire, which exceeds len(Data)
	// whenever the capture snapshot length truncated it.
	WireLength int
	// Data is the captured bytes, owned by Go.
	Data []byte
}

// LinkType identifies the link-layer header that precedes the network-layer
// header in a captured packet. Getting this wrong means parsing the IP header
// at the wrong offset and silently attributing every packet to the wrong
// protocol — one of the easier ways to ship a metric that is confidently wrong.
//
// The values match libpcap's DLT_* constants.
type LinkType int

const (
	// LinkNull is BSD loopback: a 4-byte address-family field in host byte
	// order. Capturing on macOS lo0 gives this, not Ethernet.
	LinkNull LinkType = 0
	// LinkEthernet is a 14-byte Ethernet II header (DLT_EN10MB).
	LinkEthernet LinkType = 1
	// LinkRaw is a bare IP header with no link layer at all.
	LinkRaw LinkType = 12
	// LinkLoop is OpenBSD loopback: like LinkNull but big-endian.
	LinkLoop LinkType = 108
	// LinkLinuxSLL is Linux "cooked" capture, used when capturing on the "any"
	// device: a 16-byte header ending in a 2-byte protocol field.
	LinkLinuxSLL LinkType = 113
)

func (l LinkType) String() string {
	switch l {
	case LinkNull:
		return "null"
	case LinkEthernet:
		return "ethernet"
	case LinkRaw:
		return "raw"
	case LinkLoop:
		return "loop"
	case LinkLinuxSLL:
		return "linux_sll"
	default:
		return "unknown"
	}
}

// Config describes a capture to open.
type Config struct {
	// Device is the interface to listen on ("en0", "eth0", "any"). Ignored when
	// File is set.
	Device string
	// File, when non-empty, opens a saved capture instead of a live interface.
	// This is how the package is tested: reading a live interface needs root.
	File string
	// SnapLen caps how many bytes of each packet are copied out of the kernel.
	// The metrics here only need the link + IP headers, so a small value keeps
	// the per-packet copy cheap.
	SnapLen int
	// Promiscuous puts the interface into promiscuous mode, capturing frames
	// not addressed to this host.
	Promiscuous bool
	// Timeout bounds how long a live read blocks before returning no packet.
	// It is what lets a capture loop notice a cancelled context.
	Timeout time.Duration
	// Filter is a BPF expression, in tcpdump syntax ("ip or ip6", "tcp port
	// 443"). It is compiled and applied in the kernel, so filtered-out packets
	// never cross into user space at all — which is the whole reason to set it.
	Filter string
}

// PacketFunc receives one packet during Loop. It runs on the goroutine that
// called Loop, inside a C callback, so it must not block for long.
type PacketFunc func(Packet)

// defaults for a capture that only needs to classify packets by protocol.
const (
	// defaultSnapLen is enough for an Ethernet header plus an IPv6 header plus
	// a transport header: we never look past the protocol byte.
	defaultSnapLen = 128
	// defaultTimeout keeps a live read from blocking so long that cancellation
	// feels unresponsive.
	defaultTimeout = 100 * time.Millisecond
)

func (c Config) withDefaults() Config {
	if c.SnapLen <= 0 {
		c.SnapLen = defaultSnapLen
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	return c
}
