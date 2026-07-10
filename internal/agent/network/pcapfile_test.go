package network

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Reading a live interface needs root: /dev/bpf* is root-only on macOS, and
// CAP_NET_RAW on Linux. A test suite that needs sudo is a test suite nobody
// runs, so every test here captures from a *savefile* instead — the same
// libpcap code path, the same C boundary, the same GoBytes copy, minus the
// privileges.
//
// The files are written here, in pure Go, from the classic pcap format: a
// 24-byte global header, then a 16-byte record header per packet. Writing them
// rather than committing binary fixtures keeps the test hermetic and lets each
// case craft exactly the packet it wants to talk about.

const (
	pcapMagicMicroseconds = 0xa1b2c3d4
	pcapVersionMajor      = 2
	pcapVersionMinor      = 4
	pcapGlobalHeaderLen   = 24
	pcapRecordHeaderLen   = 16
)

// testPacket is one record to write into a savefile.
type testPacket struct {
	ts         time.Time
	data       []byte
	wireLength int // 0 means len(data): not truncated
}

// writePcap writes packets into a savefile with the given link type and returns
// its path.
func writePcap(tb testing.TB, link LinkType, snapLen int, packets []testPacket) string {
	tb.Helper()

	path := filepath.Join(tb.TempDir(), "capture.pcap")
	f, err := os.Create(path)
	if err != nil {
		tb.Fatalf("create savefile: %v", err)
	}
	defer func() { _ = f.Close() }()

	var global [pcapGlobalHeaderLen]byte
	le := binary.LittleEndian
	le.PutUint32(global[0:4], pcapMagicMicroseconds)
	le.PutUint16(global[4:6], pcapVersionMajor)
	le.PutUint16(global[6:8], pcapVersionMinor)
	// [8:12] thiszone, [12:16] sigfigs — both zero, as everyone writes them.
	le.PutUint32(global[16:20], uint32(snapLen))
	le.PutUint32(global[20:24], uint32(link))
	if _, err := f.Write(global[:]); err != nil {
		tb.Fatalf("write global header: %v", err)
	}

	for i, p := range packets {
		wire := p.wireLength
		if wire == 0 {
			wire = len(p.data)
		}
		var rec [pcapRecordHeaderLen]byte
		le.PutUint32(rec[0:4], uint32(p.ts.Unix()))
		le.PutUint32(rec[4:8], uint32(p.ts.Nanosecond()/1000))
		le.PutUint32(rec[8:12], uint32(len(p.data))) // captured length
		le.PutUint32(rec[12:16], uint32(wire))       // original length on the wire
		if _, err := f.Write(rec[:]); err != nil {
			tb.Fatalf("write record %d header: %v", i, err)
		}
		if _, err := f.Write(p.data); err != nil {
			tb.Fatalf("write record %d payload: %v", i, err)
		}
	}

	if err := f.Close(); err != nil {
		tb.Fatalf("close savefile: %v", err)
	}
	return path
}

// ---------------------------------------------------------------------------
// Packet builders. Each returns the smallest frame that is still a valid input
// for the parser under test.
// ---------------------------------------------------------------------------

func ethernetFrame(etherType uint16, payload []byte) []byte {
	frame := make([]byte, 0, ethernetHdrLen+len(payload))
	frame = append(frame,
		0x02, 0x00, 0x00, 0x00, 0x00, 0x01, // dst MAC
		0x02, 0x00, 0x00, 0x00, 0x00, 0x02, // src MAC
	)
	frame = binary.BigEndian.AppendUint16(frame, etherType)
	return append(frame, payload...)
}

// vlanFrame wraps a payload in one 802.1Q tag.
func vlanFrame(innerEtherType uint16, payload []byte) []byte {
	tag := make([]byte, 0, 4)
	tag = binary.BigEndian.AppendUint16(tag, 0x0064) // priority + VLAN id 100
	tag = binary.BigEndian.AppendUint16(tag, innerEtherType)
	return ethernetFrame(etherTypeVLAN, append(tag, payload...))
}

// nullFrame is a BSD loopback frame: 4-byte address family, host byte order.
func nullFrame(af uint32, payload []byte) []byte {
	hdr := binary.LittleEndian.AppendUint32(nil, af)
	return append(hdr, payload...)
}

func ipv4Packet(proto uint8) []byte {
	pkt := make([]byte, 20)
	pkt[0] = 0x45 // version 4, IHL 5
	binary.BigEndian.PutUint16(pkt[2:4], 20)
	pkt[8] = 64    // TTL
	pkt[9] = proto // protocol
	copy(pkt[12:16], []byte{10, 0, 0, 1})
	copy(pkt[16:20], []byte{10, 0, 0, 2})
	return pkt
}

// ipv6Packet builds a fixed IPv6 header whose Next Header is nextHdr, followed
// by extra bytes (extension headers, if any).
func ipv6Packet(nextHdr uint8, extra []byte) []byte {
	pkt := make([]byte, 40)
	pkt[0] = 0x60 // version 6
	binary.BigEndian.PutUint16(pkt[4:6], uint16(len(extra)))
	pkt[6] = nextHdr
	pkt[7] = 64 // hop limit
	return append(pkt, extra...)
}
