//go:build cgo

package network

/*
#cgo LDFLAGS: -lpcap

#include <stdlib.h>
#include "pcap_shim.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

// Capture is an open libpcap handle.
//
// # Handle lifetime
//
// The *C.pcap_t is invisible to Go's garbage collector, and pcap_close frees it
// for good. If Close ran while another goroutine sat inside pcap_next_ex, that
// goroutine would return into freed memory — the classic use-after-free that a
// naive `Close()` on a C handle invites.
//
// mu prevents it. Every operation that touches the handle takes it for reading;
// Close takes it for writing, and so cannot proceed until the last reader is
// out. Close first calls pcap_breakloop, which libpcap documents as safe to call
// from a thread other than the one inside pcap_loop, so a blocking Loop is
// interrupted rather than waited on.
//
// # Handle use
//
// mu alone is not enough, and this was a real bug: an RWMutex lets two readers
// in at once, so Next and Stats could be inside libpcap simultaneously. A pcap_t
// is not thread-safe, and the agent does exactly that — its capture goroutine
// sits in Next while the collector's tick calls Stats. The result is a torn read
// of the drop counters at best.
//
// use serialises every call that actually enters libpcap. Break is the one
// exception: interrupting a blocked reader is the thing it exists for, and
// libpcap makes it safe.
type Capture struct {
	mu     sync.RWMutex // guards the handle's lifetime
	use    sync.Mutex   // serialises entry into libpcap
	handle *C.pcap_t

	// cleanup is a safety net for a caller who never calls Close. It is not a
	// destructor: it runs whenever the collector gets around to it, and never
	// at all if the process exits first. Anything that relies on a cleanup for
	// timely release is already broken.
	cleanup runtime.Cleanup

	link    LinkType
	device  string
	offline bool
}

// Open opens a live interface or, when cfg.File is set, a saved capture file.
//
// Opening a live interface needs privileges: on macOS the /dev/bpf* nodes are
// root-only, on Linux it needs CAP_NET_RAW. Failure to open is not fatal to the
// agent — the collector reports itself unavailable and the rest keeps running.
func Open(cfg Config) (*Capture, error) {
	cfg = cfg.withDefaults()

	// PCAP_ERRBUF_SIZE bytes, as libpcap requires. Passing a pointer to this Go
	// slice into C is legal: the cgo pointer rules allow Go memory to be passed
	// to C for the duration of the call, as long as the C code does not retain
	// it — and pcap only writes a message into it before returning.
	errbuf := make([]byte, C.PCAP_ERRBUF_SIZE)
	errPtr := (*C.char)(unsafe.Pointer(&errbuf[0]))

	var handle *C.pcap_t
	offline := cfg.File != ""

	if offline {
		cFile := C.CString(cfg.File)
		defer C.free(unsafe.Pointer(cFile))
		handle = C.pcap_open_offline(cFile, errPtr)
	} else {
		cDevice := C.CString(cfg.Device)
		defer C.free(unsafe.Pointer(cDevice))

		promisc := C.int(0)
		if cfg.Promiscuous {
			promisc = 1
		}
		handle = C.tf_pcap_open_live(
			cDevice,
			C.int(cfg.SnapLen),
			promisc,
			C.int(cfg.Timeout.Milliseconds()),
			errPtr,
		)
	}

	if handle == nil {
		return nil, fmt.Errorf("pcap open %s: %s", captureTarget(cfg), cString(errbuf))
	}

	c := &Capture{
		handle:  handle,
		link:    LinkType(C.pcap_datalink(handle)),
		device:  captureTarget(cfg),
		offline: offline,
	}

	// runtime.AddCleanup, not runtime.SetFinalizer: a cleanup cannot resurrect
	// the object, does not keep a cycle alive, and takes the C pointer by value
	// so it never has to reach back into the Capture that is being collected.
	c.cleanup = runtime.AddCleanup(c, func(h *C.pcap_t) { C.pcap_close(h) }, handle)

	if cfg.Filter != "" {
		if err := c.SetFilter(cfg.Filter); err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	return c, nil
}

func captureTarget(cfg Config) string {
	if cfg.File != "" {
		return cfg.File
	}
	return cfg.Device
}

// LinkType reports the link-layer header format of this capture. The parser
// needs it; guessing Ethernet is how a loopback capture becomes a graph of
// zeroes.
func (c *Capture) LinkType() LinkType { return c.link }

// Device reports what this capture was opened on.
func (c *Capture) Device() string { return c.device }

// Next returns the next packet.
//
// It returns ErrTimeout when a live capture saw nothing in time, and
// ErrEndOfCapture at the end of a savefile.
func (c *Capture) Next() (Packet, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.handle == nil {
		return Packet{}, ErrCaptureClosed
	}

	c.use.Lock()
	defer c.use.Unlock()

	var info C.tf_packet
	switch res := C.tf_pcap_next(c.handle, &info); res {
	case 1:
		// info.data points into libpcap's receive buffer, which the *next* call
		// overwrites. C.GoBytes copies it into the Go heap. Skipping this copy
		// is the single most common way to make a pcap wrapper that returns
		// yesterday's packet.
		data := C.GoBytes(unsafe.Pointer(info.data), C.int(info.caplen))
		return Packet{
			Timestamp:  time.Unix(int64(info.ts_sec), int64(info.ts_usec)*1000),
			WireLength: int(info.wirelen),
			Data:       data,
		}, nil

	case 0:
		return Packet{}, ErrTimeout

	case -2:
		return Packet{}, ErrEndOfCapture

	default:
		return Packet{}, fmt.Errorf("pcap_next_ex: %s", c.lastErrorLocked())
	}
}

// SetFilter compiles a BPF expression and installs it in the kernel, so that
// packets which do not match are never copied into user space at all. That is
// the point: filtering in Go would mean paying the copy and the CGo crossing
// for every packet on the wire, including the ones being thrown away.
func (c *Capture) SetFilter(expr string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.handle == nil {
		return ErrCaptureClosed
	}

	c.use.Lock()
	defer c.use.Unlock()

	cExpr := C.CString(expr)
	defer C.free(unsafe.Pointer(cExpr))

	if C.tf_pcap_compile_and_apply(c.handle, cExpr) != 0 {
		return fmt.Errorf("pcap filter %q: %s", expr, c.lastErrorLocked())
	}
	return nil
}

// Stats reports what libpcap and the kernel saw, which is not what this process
// received: under load the kernel drops packets before user space ever sees
// them. An agent that counts only what it read reports a quiet network during
// the exact minute the network was busiest.
//
// Only a live capture has statistics; a savefile returns an error.
func (c *Capture) Stats() (received, dropped, ifaceDropped uint64, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.handle == nil {
		return 0, 0, 0, ErrCaptureClosed
	}
	if c.offline {
		return 0, 0, 0, errors.New("pcap stats: not available for a savefile")
	}

	c.use.Lock()
	defer c.use.Unlock()

	var st C.struct_pcap_stat
	if C.pcap_stats(c.handle, &st) != 0 {
		return 0, 0, 0, fmt.Errorf("pcap_stats: %s", c.lastErrorLocked())
	}
	return uint64(st.ps_recv), uint64(st.ps_drop), uint64(st.ps_ifdrop), nil
}

// Break interrupts a Next or Loop running on another goroutine. libpcap
// documents pcap_breakloop as safe to call from a different thread than the one
// inside pcap_loop, which is what makes a capture cancellable at all — and why
// this is the one entry point that must not take the use mutex. Waiting for the
// reader it is trying to interrupt would deadlock.
func (c *Capture) Break() {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.handle != nil {
		C.pcap_breakloop(c.handle)
	}
}

// Close releases the handle. It is idempotent, and safe to call while another
// goroutine is blocked in Next or Loop: the loop is broken first, then the
// write lock waits for that goroutine to leave C before the handle is freed.
func (c *Capture) Close() error {
	c.Break()

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.handle == nil {
		return nil
	}
	// Stop the cleanup before freeing, or it may free the same handle twice.
	c.cleanup.Stop()
	C.pcap_close(c.handle)
	c.handle = nil
	return nil
}

// lastErrorLocked reads libpcap's per-handle error string. The caller must hold
// at least the read lock.
func (c *Capture) lastErrorLocked() string {
	return C.GoString(C.pcap_geterr(c.handle))
}

// cString turns a NUL-terminated C string that libpcap wrote into a Go buffer
// into a Go string. `string(buf)` would keep the trailing zeros.
func cString(buf []byte) string {
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i])
		}
	}
	return string(buf)
}

// Available reports whether this binary can capture packets. It is true here
// and false in the CGO_ENABLED=0 build.
func Available() bool { return true }

// LibraryVersion returns libpcap's version string, for the agent's startup log.
func LibraryVersion() string { return C.GoString(C.pcap_lib_version()) }
