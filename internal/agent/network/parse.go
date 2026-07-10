package network

import "encoding/binary"

// Packet parsing is pure Go on purpose. Decoding a binary header is exactly the
// work `encoding/binary` and a bounds-checked slice do well, and doing it here
// rather than in the C shim keeps the CGo surface down to "get me the bytes".
// Every byte read below is guarded by a length check: these bytes arrived from
// the network, and the fuzz target in parse_fuzz_test.go feeds them back as
// adversarially as it can.

// IP protocol numbers we classify. Everything else is counted as "other".
const (
	protoICMP   = 1
	protoTCP    = 6
	protoUDP    = 17
	protoICMPv6 = 58
)

// EtherType values.
const (
	etherTypeIPv4  = 0x0800
	etherTypeIPv6  = 0x86DD
	etherTypeVLAN  = 0x8100
	etherTypeQinQ  = 0x88A8
	ethernetHdrLen = 14
	vlanTagLen     = 4
	// maxVLANTags bounds how many stacked VLAN tags we will walk. A crafted
	// frame can otherwise claim VLAN forever and spin this loop.
	maxVLANTags = 2
)

// Address families in a BSD loopback (DLT_NULL) header. AF_INET is 2 everywhere;
// AF_INET6 is famously not — 30 on macOS/FreeBSD, 28 on OpenBSD/NetBSD, 10 on
// Linux. A capture file written on one host and read on another carries the
// writer's value, so all of them are accepted.
const (
	afInet = 2
)

var afInet6Values = [...]uint32{30, 28, 24, 10}

// IPInfo is what the collector needs from a packet: which IP version carried
// it, and which transport protocol sits inside.
type IPInfo struct {
	Version  uint8 // 4 or 6
	Protocol uint8 // IP protocol number (6 = TCP, 17 = UDP, ...)
}

// Parse extracts the IP version and transport protocol from one captured
// packet, given the link type the capture was opened with. It reports ok=false
// for anything it cannot classify — a truncated frame, a non-IP EtherType, an
// ARP packet — and never panics, whatever the bytes.
func Parse(link LinkType, data []byte) (IPInfo, bool) {
	payload, ok := stripLinkLayer(link, data)
	if !ok {
		return IPInfo{}, false
	}
	return parseIP(payload)
}

// stripLinkLayer returns the network-layer bytes, skipping the link header.
// It also validates that the link header announces an IP payload; a frame
// carrying ARP or LLDP is not an error, it is simply not ours.
func stripLinkLayer(link LinkType, data []byte) ([]byte, bool) {
	switch link {
	case LinkRaw:
		// No link layer: the IP header starts at byte zero.
		return data, true

	case LinkEthernet:
		return stripEthernet(data)

	case LinkNull:
		// 4-byte address family in the *writer's* host byte order. There is no
		// flag telling us which, so the value is read both ways and whichever
		// yields a family we recognise wins. Only one can, because the byte
		// swap of a small integer is a very large one.
		if len(data) < 4 {
			return nil, false
		}
		af := binary.LittleEndian.Uint32(data[:4])
		if !knownAF(af) {
			af = binary.BigEndian.Uint32(data[:4])
		}
		if !knownAF(af) {
			return nil, false
		}
		return data[4:], true

	case LinkLoop:
		// Same header, but the standard pins it to network byte order.
		if len(data) < 4 {
			return nil, false
		}
		if !knownAF(binary.BigEndian.Uint32(data[:4])) {
			return nil, false
		}
		return data[4:], true

	case LinkLinuxSLL:
		// 16-byte cooked header; the last two bytes are the EtherType.
		const sllHdrLen = 16
		if len(data) < sllHdrLen {
			return nil, false
		}
		switch binary.BigEndian.Uint16(data[14:16]) {
		case etherTypeIPv4, etherTypeIPv6:
			return data[sllHdrLen:], true
		}
		return nil, false

	default:
		return nil, false
	}
}

func stripEthernet(data []byte) ([]byte, bool) {
	if len(data) < ethernetHdrLen {
		return nil, false
	}
	etherType := binary.BigEndian.Uint16(data[12:14])
	offset := ethernetHdrLen

	// Walk stacked VLAN tags. Each tag is 4 bytes and republishes the
	// EtherType, so the loop is bounded rather than trusting the frame.
	for tags := 0; tags < maxVLANTags; tags++ {
		if etherType != etherTypeVLAN && etherType != etherTypeQinQ {
			break
		}
		if len(data) < offset+vlanTagLen {
			return nil, false
		}
		etherType = binary.BigEndian.Uint16(data[offset+2 : offset+4])
		offset += vlanTagLen
	}

	switch etherType {
	case etherTypeIPv4, etherTypeIPv6:
		return data[offset:], true
	}
	return nil, false
}

func knownAF(af uint32) bool {
	if af == afInet {
		return true
	}
	for _, v := range afInet6Values {
		if af == v {
			return true
		}
	}
	return false
}

// parseIP reads the version nibble and the protocol field. For IPv6 it walks the
// extension-header chain, because the transport protocol of a packet carrying a
// hop-by-hop or routing header is not in the fixed header's Next Header field —
// counting those as their extension type is how an agent reports a fleet full of
// protocol 0.
func parseIP(data []byte) (IPInfo, bool) {
	if len(data) < 1 {
		return IPInfo{}, false
	}

	switch data[0] >> 4 {
	case 4:
		const ipv4MinHdr = 20
		if len(data) < ipv4MinHdr {
			return IPInfo{}, false
		}
		return IPInfo{Version: 4, Protocol: data[9]}, true

	case 6:
		const ipv6HdrLen = 40
		if len(data) < ipv6HdrLen {
			return IPInfo{}, false
		}
		next := data[6]
		rest := data[ipv6HdrLen:]
		proto, ok := skipIPv6Extensions(next, rest)
		if !ok {
			return IPInfo{}, false
		}
		return IPInfo{Version: 6, Protocol: proto}, true
	}
	return IPInfo{}, false
}

// IPv6 extension headers we know how to skip. Each begins with a Next Header
// byte and a length byte measured in 8-octet units, not counting the first 8.
const (
	ipv6ExtHopByHop     = 0
	ipv6ExtRouting      = 43
	ipv6ExtFragment     = 44
	ipv6ExtDestOpts     = 60
	ipv6NoNextHeader    = 59
	maxIPv6ExtHeaders   = 8 // a bound, so a crafted chain cannot loop forever
	ipv6FragmentHdrSize = 8
)

func skipIPv6Extensions(next uint8, rest []byte) (uint8, bool) {
	for i := 0; i < maxIPv6ExtHeaders; i++ {
		switch next {
		case ipv6ExtHopByHop, ipv6ExtRouting, ipv6ExtDestOpts:
			if len(rest) < 2 {
				return 0, false
			}
			hdrLen := (int(rest[1]) + 1) * 8
			if len(rest) < hdrLen {
				return 0, false
			}
			next = rest[0]
			rest = rest[hdrLen:]
		case ipv6ExtFragment:
			// The fragment header has a fixed size and no length field.
			if len(rest) < ipv6FragmentHdrSize {
				return 0, false
			}
			next = rest[0]
			rest = rest[ipv6FragmentHdrSize:]
		case ipv6NoNextHeader:
			return 0, false
		default:
			return next, true
		}
	}
	// A chain longer than the bound is not a packet we will classify.
	return 0, false
}

// ProtocolName maps an IP protocol number onto the label the metrics use.
// Anything unclassified collapses into "other" rather than exploding the label
// cardinality with one series per protocol number seen on the wire.
func ProtocolName(proto uint8) string {
	switch proto {
	case protoTCP:
		return "tcp"
	case protoUDP:
		return "udp"
	case protoICMP, protoICMPv6:
		return "icmp"
	default:
		return "other"
	}
}
