//go:build cgo

package network

/*
#include <stdint.h>
#include "pcap_shim.h"
*/
import "C"

import (
	"fmt"
	"runtime/cgo"
	"time"
	"unsafe"
)

// Callback-style APIs are how most C libraries stream data: "here is a function
// pointer, call it for every packet." Getting one to call back into Go is the
// second hard problem of CGo, after memory ownership.
//
// The naive move — hand C a pointer to a Go closure, or to any Go object, and
// let it give the pointer back later — is exactly what the cgo pointer rules
// forbid. C is allowed to hold a Go pointer only for the duration of the call;
// keeping it across calls means the garbage collector may free or move the
// object underneath it, and the runtime's cgocheck will (usually) panic to say
// so.
//
// The fix is an *opaque handle*: register the Go value in a table, pass C the
// integer index, and look it up again when C calls back. Every C-callback
// binding in Go does this. Since Go 1.17 the table is in the standard library
// as runtime/cgo.Handle, so this package does not hand-roll one — a hand-rolled
// registry is a map, a mutex, and a monotonic counter, which is precisely what
// cgo.Handle already is, minus the bugs.

// loopState is what the exported handler needs to turn a raw C packet into a Go
// one. It is reachable from the cgo.Handle table, so the GC keeps it alive for
// exactly as long as C might call back.
//
// handle is carried here so a panicking callback can stop the loop it is inside.
// Recording an error and returning is not enough: pcap_loop does not read Go
// variables, and on a live interface it never runs out of packets.
type loopState struct {
	fn     PacketFunc
	handle *C.pcap_t
	err    error // set when the callback panicked
}

// tfPacketHandler is called by libpcap, on the goroutine that called Loop, once
// per packet. Its signature must be C-compatible: cgo will not export a
// function taking a Go string or slice.
//
// It must not panic. A panic here unwinds through a C stack frame, which is
// undefined behaviour, so the recover is installed *before* the handle lookup:
// cgo.Handle.Value panics on an invalid handle, and is therefore the one call
// here most able to unwind into libpcap.
//
//export tfPacketHandler
func tfPacketHandler(user *C.uchar, hdr *C.struct_pcap_pkthdr, data *C.uchar) {
	// `user` is not a pointer; it is the cgo.Handle integer that tf_pcap_loop
	// cast into the u_char* slot pcap_loop insists on. Converting a pointer
	// back to a uintptr is always allowed.
	h := cgo.Handle(uintptr(unsafe.Pointer(user)))

	var state *loopState
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if state == nil {
			// The handle itself was bad: nowhere to record the error, and no
			// pcap_t to break with. Swallowing the panic is still better than
			// letting it unwind through libpcap.
			return
		}
		state.err = fmt.Errorf("packet callback panicked: %v", r)

		// Stop the loop. Without this, pcap_loop keeps dispatching — each later
		// packet hits the early return below — and Loop never returns the error
		// it is holding, while still owning the read lock Close needs. A savefile
		// ends and hides it; a live interface does not.
		C.pcap_breakloop(state.handle)
	}()

	var ok bool
	state, ok = h.Value().(*loopState)
	if !ok || state == nil {
		return
	}
	if state.err != nil {
		return // an earlier packet panicked; the breakloop is already in flight
	}

	// Same rule as the synchronous path: libpcap owns these bytes only until it
	// returns to its own loop, so copy before the callback can retain them.
	pkt := Packet{
		Timestamp:  time.Unix(int64(hdr.ts.tv_sec), int64(hdr.ts.tv_usec)*1000),
		WireLength: int(hdr.len),
		Data:       C.GoBytes(unsafe.Pointer(data), C.int(hdr.caplen)),
	}
	state.fn(pkt)
}

// Loop delivers up to count packets to fn, blocking until that many have
// arrived, the savefile ends, or Break is called. count <= 0 means "until
// broken".
//
// Loop is measurably *cheaper* per packet than a Next loop (see
// BenchmarkCaptureCallbackPerPacket), because it crosses into C once and lets
// libpcap call back, rather than crossing once per packet. What it gives up is
// control: fn runs inside a C stack frame, so it must not block, and
// cancellation has to come through Break rather than a select. The collector
// runs the Next loop for exactly that reason.
func (c *Capture) Loop(count int, fn PacketFunc) error {
	if fn == nil {
		return fmt.Errorf("network: Loop requires a callback")
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.handle == nil {
		return ErrCaptureClosed
	}

	// Serialise entry into libpcap with Next/Stats/SetFilter. Break deliberately
	// does not take this, or it could never interrupt the loop it holds.
	c.use.Lock()
	defer c.use.Unlock()

	state := &loopState{fn: fn, handle: c.handle}
	h := cgo.NewHandle(state)
	// The handle must outlive every callback and then be released, or the table
	// grows forever. Delete is the C-free of this abstraction.
	defer h.Delete()

	res := C.tf_pcap_loop(c.handle, C.int(count), C.uintptr_t(h))

	if state.err != nil {
		return state.err
	}
	switch res {
	case 0: // count reached, or savefile exhausted
		return nil
	case -2: // pcap_breakloop
		return nil
	case -1:
		return fmt.Errorf("pcap_loop: %s", c.lastErrorLocked())
	default:
		return nil
	}
}
