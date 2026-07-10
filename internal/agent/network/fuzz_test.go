package network

import "testing"

// The parser is the one place in this project that reads bytes chosen by
// whoever is on the network. It runs on every packet, inside an agent that runs
// as root on every host — the least forgiving place in the system for an
// out-of-range slice.
//
// The invariant is total: for any link type and any bytes, Parse returns and
// does not panic. Everything it cannot classify it must decline, not guess.
// fuzzLinks is what the fuzzer's first argument indexes. The seeds below pass an
// *index into this slice*, not a DLT value: passing int(LinkRaw)==12 would land,
// after the modulo, on fuzzLinks[0] — so the raw-IPv6 seed would have seeded the
// loopback branch instead, and the Linux-SLL seed the unknown-link branch. The
// corpus would look complete and cover the wrong things.
var fuzzLinks = []LinkType{LinkNull, LinkEthernet, LinkRaw, LinkLoop, LinkLinuxSLL, 999}

const (
	seedNull     = 0
	seedEthernet = 1
	seedRaw      = 2
	seedLinuxSLL = 4
	seedUnknown  = 5
)

func FuzzParse(f *testing.F) {
	f.Add(seedEthernet, ethernetFrame(etherTypeIPv4, ipv4Packet(protoTCP)))
	f.Add(seedEthernet, ethernetFrame(etherTypeIPv6, ipv6Packet(protoUDP, nil)))
	f.Add(seedEthernet, vlanFrame(etherTypeIPv4, ipv4Packet(protoICMP)))
	f.Add(seedNull, nullFrame(afInet, ipv4Packet(protoUDP)))
	f.Add(seedRaw, ipv6Packet(ipv6ExtHopByHop, []byte{protoTCP, 0, 0, 0, 0, 0, 0, 0}))
	f.Add(seedLinuxSLL, append(append(make([]byte, 14), 0x08, 0x00), ipv4Packet(protoTCP)...))
	f.Add(seedUnknown, ipv4Packet(protoTCP))
	f.Add(seedEthernet, []byte{})

	f.Fuzz(func(t *testing.T, linkIdx int, data []byte) {
		// Map the fuzzer's int onto a link type, keeping the unknown one in the
		// rotation so its rejection path is exercised too.
		link := fuzzLinks[uint(linkIdx)%uint(len(fuzzLinks))]

		info, ok := Parse(link, data)
		if !ok {
			return
		}

		// A packet it *did* classify must be classified coherently: only IPv4
		// and IPv6 exist, and the protocol must map to a stable label.
		if info.Version != 4 && info.Version != 6 {
			t.Fatalf("Parse accepted a packet with IP version %d: link=%v data=%x", info.Version, link, data)
		}
		switch name := ProtocolName(info.Protocol); name {
		case "tcp", "udp", "icmp", "other":
		default:
			t.Fatalf("ProtocolName(%d) = %q, which is not a label the collector emits", info.Protocol, name)
		}
	})
}

// Parse must be a pure function of its inputs: the collector calls it on a
// buffer it owns, and a parser that mutated the packet would corrupt the bytes
// any future decoder sees.
func FuzzParseDoesNotMutateItsInput(f *testing.F) {
	f.Add(ethernetFrame(etherTypeIPv4, ipv4Packet(protoTCP)))
	f.Add(vlanFrame(etherTypeIPv6, ipv6Packet(protoTCP, nil)))
	f.Add([]byte{0x45, 0x00})

	f.Fuzz(func(t *testing.T, data []byte) {
		original := make([]byte, len(data))
		copy(original, data)

		for _, link := range []LinkType{LinkEthernet, LinkNull, LinkRaw, LinkLoop, LinkLinuxSLL} {
			Parse(link, data)
		}

		for i := range original {
			if data[i] != original[i] {
				t.Fatalf("Parse mutated its input at byte %d: %x -> %x", i, original, data)
			}
		}
	})
}

// The seed constants are indices into fuzzLinks. If someone reorders that slice,
// every seed silently starts exercising a different branch — the failure mode
// this test exists to make loud.
func TestFuzzSeedIndicesNameTheirLinkTypes(t *testing.T) {
	want := map[int]LinkType{
		seedNull:     LinkNull,
		seedEthernet: LinkEthernet,
		seedRaw:      LinkRaw,
		seedLinuxSLL: LinkLinuxSLL,
	}
	for idx, link := range want {
		if fuzzLinks[idx] != link {
			t.Errorf("seed index %d names %v, want %v", idx, fuzzLinks[idx], link)
		}
	}
	if fuzzLinks[seedUnknown].String() != "unknown" {
		t.Errorf("seedUnknown must name an unrecognised link type, got %v", fuzzLinks[seedUnknown])
	}
}
