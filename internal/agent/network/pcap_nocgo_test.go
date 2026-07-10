//go:build !cgo

package network

import (
	"errors"
	"testing"
)

// Under CGO_ENABLED=0 every capture test vanishes with the cgo build tag, so
// until this file existed nothing asserted the stub's contract at all: `make
// test-nocgo` was a compile check and a parser suite. Someone could have made
// Open succeed, or Available return true, and only the agent's startup log would
// have noticed.
//
// The contract is small and worth pinning: the stub never pretends to capture,
// and it never panics on a nil handle.

func TestStubReportsCaptureUnavailable(t *testing.T) {
	if Available() {
		t.Error("Available must be false in a build without CGo")
	}
	if _, err := Open(Config{Device: "eth0"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Open: want ErrUnsupported, got %v", err)
	}
	if _, err := Open(Config{File: "capture.pcap"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Open(file): want ErrUnsupported, got %v", err)
	}
	if _, err := NewCollector(Config{Device: "eth0"}, nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("NewCollector: want ErrUnsupported, got %v", err)
	}
}

// The agent logs LibraryVersion() at startup. It must explain the absence rather
// than claim a libpcap that is not linked in.
func TestStubLibraryVersionExplainsItself(t *testing.T) {
	if v := LibraryVersion(); v == "" {
		t.Fatal("LibraryVersion must say why capture is unavailable")
	}
}

// Every method must be safe on the zero Capture: the stub hands one back nowhere,
// but a caller who constructs one must not crash the agent.
func TestStubMethodsAreSafeAndRefuse(t *testing.T) {
	var c Capture

	if _, err := c.Next(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Next: want ErrUnsupported, got %v", err)
	}
	if err := c.SetFilter("tcp"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("SetFilter: want ErrUnsupported, got %v", err)
	}
	if err := c.Loop(1, func(Packet) {}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Loop: want ErrUnsupported, got %v", err)
	}
	if _, _, _, err := c.Stats(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Stats: want ErrUnsupported, got %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close on the stub must succeed, got %v", err)
	}
	c.Break() // must not panic
}
