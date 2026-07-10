//go:build !cgo

package network

// This file is what the package becomes when the binary is built with
// CGO_ENABLED=0.
//
// The moment a project takes a CGo dependency, it loses three things: a
// single-command cross-compile (`GOOS=linux GOARCH=arm64 go build` now needs a
// C toolchain and libpcap headers for the *target*), a fully static binary, and
// the ability to build at all on a machine without the C library installed.
//
// Confining CGo behind a build tag buys them back. The default `go build`
// produces an agent with packet capture; `CGO_ENABLED=0 go build` produces one
// that cross-compiles anywhere, links statically, and reports the network
// collector as unavailable instead of refusing to compile. The rest of the
// agent — CPU, memory, disk, uptime — does not notice.
//
// Graceful degradation, not a build error: an operator who wants a static
// binary for a scratch container should not have to learn what libpcap is.

// Capture is the no-op form of a packet capture. Every method reports
// ErrUnsupported.
type Capture struct{}

// Open always fails without CGo.
func Open(_ Config) (*Capture, error) { return nil, ErrUnsupported }

func (c *Capture) LinkType() LinkType { return LinkEthernet }
func (c *Capture) Device() string     { return "" }

func (c *Capture) Next() (Packet, error)  { return Packet{}, ErrUnsupported }
func (c *Capture) SetFilter(string) error { return ErrUnsupported }
func (c *Capture) Loop(int, PacketFunc) error {
	return ErrUnsupported
}
func (c *Capture) Break()       {}
func (c *Capture) Close() error { return nil }

func (c *Capture) Stats() (received, dropped, ifaceDropped uint64, err error) {
	return 0, 0, 0, ErrUnsupported
}

// Available reports whether this binary can capture packets: false here.
func Available() bool { return false }

// LibraryVersion says why capture is unavailable, for the startup log.
func LibraryVersion() string { return "libpcap unavailable (built without CGo)" }
