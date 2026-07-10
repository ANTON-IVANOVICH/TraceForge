package network

import (
	"encoding/binary"
	"testing"
)

func TestParseLinkLayers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		link     LinkType
		data     []byte
		wantOK   bool
		wantVer  uint8
		wantProt uint8
	}{
		{
			name:     "ethernet ipv4 tcp",
			link:     LinkEthernet,
			data:     ethernetFrame(etherTypeIPv4, ipv4Packet(protoTCP)),
			wantOK:   true,
			wantVer:  4,
			wantProt: protoTCP,
		},
		{
			name:     "ethernet ipv6 udp",
			link:     LinkEthernet,
			data:     ethernetFrame(etherTypeIPv6, ipv6Packet(protoUDP, nil)),
			wantOK:   true,
			wantVer:  6,
			wantProt: protoUDP,
		},
		{
			name:     "ethernet with one vlan tag",
			link:     LinkEthernet,
			data:     vlanFrame(etherTypeIPv4, ipv4Packet(protoICMP)),
			wantOK:   true,
			wantVer:  4,
			wantProt: protoICMP,
		},
		{
			// The frame libpcap hands back from macOS lo0. Parsing it as
			// Ethernet reads the IP header 10 bytes late and reports garbage.
			name:     "bsd loopback ipv4",
			link:     LinkNull,
			data:     nullFrame(afInet, ipv4Packet(protoUDP)),
			wantOK:   true,
			wantVer:  4,
			wantProt: protoUDP,
		},
		{
			name:     "bsd loopback ipv6 with the macOS address family",
			link:     LinkNull,
			data:     nullFrame(30, ipv6Packet(protoTCP, nil)),
			wantOK:   true,
			wantVer:  6,
			wantProt: protoTCP,
		},
		{
			// A savefile written on Linux carries AF_INET6 = 10, not 30.
			name:     "bsd loopback ipv6 with the Linux address family",
			link:     LinkNull,
			data:     nullFrame(10, ipv6Packet(protoTCP, nil)),
			wantOK:   true,
			wantVer:  6,
			wantProt: protoTCP,
		},
		{
			name:     "openbsd loop header is big-endian",
			link:     LinkLoop,
			data:     append(binary.BigEndian.AppendUint32(nil, afInet), ipv4Packet(protoTCP)...),
			wantOK:   true,
			wantVer:  4,
			wantProt: protoTCP,
		},
		{
			name:     "raw ip needs no link header",
			link:     LinkRaw,
			data:     ipv4Packet(protoICMP),
			wantOK:   true,
			wantVer:  4,
			wantProt: protoICMP,
		},
		{
			name:     "linux cooked capture",
			link:     LinkLinuxSLL,
			data:     append(append(make([]byte, 14), 0x08, 0x00), ipv4Packet(protoUDP)...),
			wantOK:   true,
			wantVer:  4,
			wantProt: protoUDP,
		},
		{
			name:   "arp is not an error, just not ours",
			link:   LinkEthernet,
			data:   ethernetFrame(0x0806, make([]byte, 28)),
			wantOK: false,
		},
		{
			name:   "truncated ethernet header",
			link:   LinkEthernet,
			data:   []byte{1, 2, 3},
			wantOK: false,
		},
		{
			name:   "ethernet header but truncated ipv4",
			link:   LinkEthernet,
			data:   ethernetFrame(etherTypeIPv4, []byte{0x45, 0x00}),
			wantOK: false,
		},
		{
			name:   "empty packet",
			link:   LinkEthernet,
			data:   nil,
			wantOK: false,
		},
		{
			name:   "unknown link type",
			link:   LinkType(999),
			data:   ipv4Packet(protoTCP),
			wantOK: false,
		},
		{
			name:   "loopback header with an unknown address family",
			link:   LinkNull,
			data:   nullFrame(0xDEAD, ipv4Packet(protoTCP)),
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := Parse(tt.link, tt.data)
			if ok != tt.wantOK {
				t.Fatalf("ok: want %v, got %v (info %+v)", tt.wantOK, ok, got)
			}
			if !ok {
				return
			}
			if got.Version != tt.wantVer {
				t.Errorf("IP version: want %d, got %d", tt.wantVer, got.Version)
			}
			if got.Protocol != tt.wantProt {
				t.Errorf("protocol: want %d, got %d", tt.wantProt, got.Protocol)
			}
		})
	}
}

// A packet carrying an IPv6 hop-by-hop or routing header does not name its
// transport protocol in the fixed header. Reading the Next Header field and
// stopping there reports a fleet full of protocol 0, and no TCP at all.
func TestParseWalksIPv6ExtensionHeaders(t *testing.T) {
	t.Parallel()

	// Hop-by-hop (type 0), 8 bytes: next header = TCP, len = 0 (means 8 bytes).
	hopByHop := []byte{protoTCP, 0, 0, 0, 0, 0, 0, 0}
	pkt := ethernetFrame(etherTypeIPv6, ipv6Packet(ipv6ExtHopByHop, hopByHop))

	got, ok := Parse(LinkEthernet, pkt)
	if !ok {
		t.Fatal("a packet with a hop-by-hop header should still classify")
	}
	if got.Protocol != protoTCP {
		t.Errorf("protocol behind a hop-by-hop header: want TCP(%d), got %d", protoTCP, got.Protocol)
	}
}

func TestParseIPv6FragmentHeaderHasNoLengthField(t *testing.T) {
	t.Parallel()

	// The fragment header is fixed at 8 bytes; its second byte is reserved, not
	// a length. Treating it as a length walks off into the payload.
	fragment := []byte{protoUDP, 0xFF, 0, 0, 0, 0, 0, 0}
	pkt := ipv6Packet(ipv6ExtFragment, fragment)

	got, ok := Parse(LinkRaw, pkt)
	if !ok {
		t.Fatal("a fragmented packet should classify")
	}
	if got.Protocol != protoUDP {
		t.Errorf("protocol behind a fragment header: want UDP(%d), got %d", protoUDP, got.Protocol)
	}
}

// A crafted chain of extension headers must not spin the parser. Each header
// says "the next one is also an extension header", forever.
func TestParseRejectsAnEndlessExtensionChain(t *testing.T) {
	t.Parallel()

	var chain []byte
	for i := 0; i < maxIPv6ExtHeaders+4; i++ {
		chain = append(chain, ipv6ExtDestOpts, 0, 0, 0, 0, 0, 0, 0)
	}
	pkt := ipv6Packet(ipv6ExtDestOpts, chain)

	if _, ok := Parse(LinkRaw, pkt); ok {
		t.Error("an extension-header chain longer than the bound must not classify")
	}
}

// A VLAN tag whose inner EtherType is another VLAN tag, repeated, is the same
// attack against stripEthernet.
func TestParseBoundsStackedVLANTags(t *testing.T) {
	t.Parallel()

	payload := ipv4Packet(protoTCP)
	// Two tags is legal (QinQ) and must parse.
	inner := vlanFrame(etherTypeIPv4, payload)
	if _, ok := Parse(LinkEthernet, inner); !ok {
		t.Error("a single VLAN tag must parse")
	}

	// Build a frame with more tags than the bound allows.
	var tags []byte
	for i := 0; i < maxVLANTags+2; i++ {
		tags = binary.BigEndian.AppendUint16(tags, 0x0064)
		tags = binary.BigEndian.AppendUint16(tags, etherTypeVLAN)
	}
	frame := ethernetFrame(etherTypeVLAN, append(tags, payload...))
	if _, ok := Parse(LinkEthernet, frame); ok {
		t.Error("a VLAN stack deeper than the bound must not classify")
	}
}

func TestProtocolName(t *testing.T) {
	t.Parallel()

	tests := map[uint8]string{
		protoTCP:    "tcp",
		protoUDP:    "udp",
		protoICMP:   "icmp",
		protoICMPv6: "icmp",
		89:          "other", // OSPF
		0:           "other",
	}
	for proto, want := range tests {
		if got := ProtocolName(proto); got != want {
			t.Errorf("ProtocolName(%d) = %q, want %q", proto, got, want)
		}
	}
}

func TestLinkTypeString(t *testing.T) {
	t.Parallel()
	if got := LinkEthernet.String(); got != "ethernet" {
		t.Errorf("LinkEthernet: got %q", got)
	}
	if got := LinkType(999).String(); got != "unknown" {
		t.Errorf("unknown link type: got %q", got)
	}
}
