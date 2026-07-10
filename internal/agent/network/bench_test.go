//go:build cgo

package network

import (
	"fmt"
	"testing"
	"time"
)

// The first thing to know about CGo is what a call costs, because it decides
// the shape of every binding you write. A Go call is about a nanosecond. A CGo
// call is tens of them: the runtime must move the goroutine onto a system
// stack, tell the scheduler this thread is leaving Go, and undo it on the way
// back. Nothing in the C function is being measured here — tf_add is one
// instruction.
//
// The consequence, and the whole design rule for a CGo binding: cross the
// boundary rarely and do a lot of work on the far side. A wrapper that calls
// into C once per byte is slower than the pure-Go code it replaced, however
// fast the C is. This is why go-sqlite3 loses to a pure-Go SQLite on short
// queries, and why this package's C shim returns a whole packet per call rather
// than a field per call.
//
// These benchmarks live behind the same `cgo` build tag as the code they
// measure, and go through the tiny helpers in boundary_cgo.go, because cgo is
// not permitted in a _test.go file.

var (
	sinkInt    int
	sinkInt64  int64
	sinkBool   bool
	sinkString string
	sinkBytes  []byte
	sinkPacket Packet
)

func goAdd(a, b int) int { return a + b }

func BenchmarkCallOverheadGo(b *testing.B) {
	sum := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sum = goAdd(sum, 1)
	}
	sinkInt = sum
}

func BenchmarkCallOverheadCGo(b *testing.B) {
	sum := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sum = cAdd(sum, 1)
	}
	sinkInt = sum
}

// Handing C a buffer: three ways, three costs.
//
//   - copy_to_c_heap: C.CBytes malloc's and memcpy's. Correct always, and the
//     only option when C keeps the pointer after returning. Costs a malloc, a
//     copy and a free per call, and the copy grows with the buffer.
//   - go_pointer: pass &buf[0] straight through. Legal for the duration of the
//     call, because a []byte's backing array holds no Go pointers. Free.
//   - pinned_go_pointer: the same, wrapped in a runtime.Pinner — which this
//     call does not need. Included to price a common misunderstanding.
func BenchmarkPassBufferToC(b *testing.B) {
	for _, size := range []int{64, 1500, 65536} {
		buf := make([]byte, size)
		for i := range buf {
			buf[i] = byte(i)
		}

		b.Run(fmt.Sprintf("copy_to_c_heap/%d", size), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p := cCopyToCHeap(buf)
				sinkInt64 = cSumCBuffer(p, size)
				cFree(p)
			}
		})

		b.Run(fmt.Sprintf("go_pointer/%d", size), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sinkInt64 = cSumGoBuffer(buf)
			}
		})

		b.Run(fmt.Sprintf("pinned_go_pointer/%d", size), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sinkInt64 = cSumPinnedGoBuffer(buf)
			}
		})
	}
}

// The reverse direction. C.GoBytes allocates a Go slice and copies into it,
// once per packet — the copy this package cannot skip, because libpcap reuses
// its receive buffer. It is the per-packet floor of any pcap wrapper in Go.
func BenchmarkGoBytesCopy(b *testing.B) {
	for _, size := range []int{64, 128, 1500} {
		p := cMalloc(size)
		b.Run(fmt.Sprintf("%d", size), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sinkBytes = cGoBytes(p, size)
			}
		})
		cFree(p)
	}
}

// String conversion across the boundary: both directions copy, and CString's
// result has to be freed by hand — every one that is not is a leak in a process
// designed to run for months.
func BenchmarkStringConversion(b *testing.B) {
	const filter = "tcp port 443 and host 10.0.0.1"

	b.Run("CString+free", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			p := cCString(filter)
			cFree(p)
		}
	})

	cs := cCString(filter)
	defer cFree(cs)
	b.Run("GoString", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkString = cGoString(cs)
		}
	})
}

// benchPackets is large enough that reopening the exhausted savefile is rare:
// at 4096 packets an Open cost of tens of microseconds amortised to ~8ns/packet,
// which is a third of the difference these two benchmarks exist to measure.
const benchPackets = 200_000

// benchCapture writes a savefile of n identical TCP-over-Ethernet frames.
func benchCapture(b *testing.B, n int) string {
	b.Helper()
	packets := make([]testPacket, n)
	frame := ethernetFrame(etherTypeIPv4, ipv4Packet(protoTCP))
	for i := range packets {
		packets[i] = testPacket{ts: captureBase.Add(time.Duration(i) * time.Microsecond), data: frame}
	}
	return writePcap(b, LinkEthernet, 65536, packets)
}

// The whole per-packet cost of the synchronous path: one CGo call plus one
// GoBytes copy. Reopening the exhausted savefile is setup, not measurement; see
// benchPackets for why it is sized the way it is.
func BenchmarkCapturePerPacket(b *testing.B) {
	path := benchCapture(b, benchPackets)

	b.ReportAllocs()
	b.ResetTimer()

	for read := 0; read < b.N; {
		c, err := Open(Config{File: path})
		if err != nil {
			b.Fatal(err)
		}
		for read < b.N {
			pkt, err := c.Next()
			if err != nil {
				break // end of savefile: reopen
			}
			sinkPacket = pkt
			read++
		}
		_ = c.Close()
	}
}

// The same packets through the callback path.
//
// The received wisdom is that a C→Go callback is dearer than a Go→C call, so
// the callback path should lose. On this machine it wins, by about the cost of
// one CGo call per packet (≈153ns vs ≈128ns, 2 allocs vs 1):
//
//	Next   — one Go→C call *per packet*, each paying the full crossing and an
//	         argument frame.
//	Loop   — one Go→C call for the whole loop, then one C→Go callback per
//	         packet, which turns out to be the cheaper of the two crossings.
//
// The lesson is not "callbacks are fast". It is that the number of crossings is
// what matters, not their direction: pcap_loop amortises the expensive one over
// every packet it delivers. Measure before you believe either way — this
// comment originally claimed the opposite, and the benchmark corrected it.
func BenchmarkCaptureCallbackPerPacket(b *testing.B) {
	path := benchCapture(b, benchPackets)

	b.ReportAllocs()
	b.ResetTimer()

	for read := 0; read < b.N; {
		c, err := Open(Config{File: path})
		if err != nil {
			b.Fatal(err)
		}
		got := 0
		_ = c.Loop(b.N-read, func(p Packet) {
			sinkPacket = p
			got++
		})
		_ = c.Close()
		if got == 0 {
			b.Fatal("callback loop made no progress")
		}
		read += got
	}
}

// The pure-Go half, for scale: classifying a packet costs a fraction of what
// getting it across the boundary does. Optimizing the parser before the
// boundary would be optimizing the wrong thing.
func BenchmarkParsePacket(b *testing.B) {
	cases := []struct {
		name  string
		link  LinkType
		frame []byte
	}{
		{"ethernet_ipv4", LinkEthernet, ethernetFrame(etherTypeIPv4, ipv4Packet(protoTCP))},
		{"ethernet_ipv6", LinkEthernet, ethernetFrame(etherTypeIPv6, ipv6Packet(protoTCP, nil))},
		{"vlan_ipv4", LinkEthernet, vlanFrame(etherTypeIPv4, ipv4Packet(protoUDP))},
		{"ipv6_ext_hbh", LinkRaw, ipv6Packet(ipv6ExtHopByHop, []byte{protoTCP, 0, 0, 0, 0, 0, 0, 0})},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, ok := Parse(tc.link, tc.frame)
				sinkBool = ok
			}
		})
	}
}
