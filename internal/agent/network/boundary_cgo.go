//go:build cgo

package network

/*
#include <stdlib.h>
#include "pcap_shim.h"
*/
import "C"

import (
	"runtime"
	"unsafe"
)

// The helpers below exist so that bench_test.go can measure the CGo boundary
// itself — the cost of crossing it, and the cost of the three ways to hand C a
// buffer.
//
// They live in a normal file rather than in the test, because cgo is not
// supported in _test.go files: `import "C"` there is a compile error. The
// alternative, a separate package of benchmark shims, would put them in the
// production import graph for real. Unexported and unused by anything but the
// benchmark, the linker drops them from the agent binary.
//
// Every one of them is one line. That is the point: the numbers they produce
// are the boundary, not the work.

// cAdd crosses into C to add two integers. Whatever this benchmarks, it is not
// addition.
func cAdd(a, b int) int { return int(C.tf_add(C.int(a), C.int(b))) }

// cSumCBuffer sums a buffer that already lives in the C heap.
func cSumCBuffer(p unsafe.Pointer, n int) int64 {
	return int64(C.tf_sum_bytes((*C.uchar)(p), C.int(n)))
}

// cSumGoBuffer passes a pointer into the Go heap straight to C.
//
// This is legal, and it is the cheapest correct option for a synchronous call:
// the cgo pointer rules allow Go memory to be passed to C for the duration of a
// call, provided the memory contains no Go pointers. A []byte's backing array
// contains none.
func cSumGoBuffer(buf []byte) int64 {
	if len(buf) == 0 {
		return 0
	}
	n := C.tf_sum_bytes((*C.uchar)(unsafe.Pointer(&buf[0])), C.int(len(buf)))
	// The compiler must not decide buf is dead before C is done reading it.
	runtime.KeepAlive(buf)
	return int64(n)
}

// cSumPinnedGoBuffer does the same through a runtime.Pinner.
//
// Pinning is widely repeated as "how you safely pass Go memory to C". It is not
// what makes the call above safe — that call is safe without it. Pin exists for
// the cases the pointer rules forbid: C retaining the pointer past the call, or
// a Go pointer nested inside another Go object. Using it where it is not needed
// buys nothing and costs a pin table entry.
func cSumPinnedGoBuffer(buf []byte) int64 {
	if len(buf) == 0 {
		return 0
	}
	var pinner runtime.Pinner
	pinner.Pin(&buf[0])
	defer pinner.Unpin()
	return int64(C.tf_sum_bytes((*C.uchar)(unsafe.Pointer(&buf[0])), C.int(len(buf))))
}

// cCopyToCHeap mallocs and memcpys buf into the C heap. The caller must cFree.
func cCopyToCHeap(buf []byte) unsafe.Pointer { return C.CBytes(buf) }

func cMalloc(n int) unsafe.Pointer { return C.malloc(C.size_t(n)) }
func cFree(p unsafe.Pointer)       { C.free(p) }

// cGoBytes copies n bytes out of the C heap into a fresh Go slice — the copy
// this package performs once per captured packet.
func cGoBytes(p unsafe.Pointer, n int) []byte { return C.GoBytes(p, C.int(n)) }

// cCString allocates a NUL-terminated copy of s in the C heap.
func cCString(s string) unsafe.Pointer { return unsafe.Pointer(C.CString(s)) }

// cGoString copies a NUL-terminated C string back into a Go string.
func cGoString(p unsafe.Pointer) string { return C.GoString((*C.char)(p)) }
