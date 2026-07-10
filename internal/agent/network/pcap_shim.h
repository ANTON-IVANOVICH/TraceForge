/*
 * Thin C wrappers over the parts of libpcap that CGo handles badly.
 *
 * Two reasons this file exists rather than a C block inside a Go comment:
 *
 *  1. `struct pcap_pkthdr` embeds a `struct timeval`, whose layout differs
 *     across platforms, and libpcap's headers carry unions that cgo cannot
 *     translate. Flattening what we need into a plain struct with fixed-width
 *     fields removes that whole class of portability problem.
 *
 *  2. A Go file that uses `//export` may not define C functions in its
 *     preamble — only declare them. The packet callback below is exported from
 *     Go, so its C side has to live in a real translation unit.
 */

#ifndef TRACEFORGE_PCAP_SHIM_H
#define TRACEFORGE_PCAP_SHIM_H

#include <pcap.h>

/*
 * tf_packet is what one captured packet looks like on the way back to Go.
 *
 * `data` points into libpcap's own receive buffer, which it reuses on the very
 * next call. The Go side copies it out immediately; nothing here owns it.
 */
typedef struct {
    int                  caplen;   /* bytes actually captured */
    int                  wirelen;  /* bytes on the wire, >= caplen */
    long long            ts_sec;
    long long            ts_usec;
    const unsigned char *data;
} tf_packet;

/*
 * tf_pcap_next fetches the next packet.
 *
 * Returns 1 on success, 0 on a live-capture timeout, -1 on error and -2 at the
 * end of a savefile — libpcap's own pcap_next_ex contract, passed through.
 */
int tf_pcap_next(pcap_t *handle, tf_packet *out);

/*
 * tf_pcap_loop dispatches up to `count` packets (0 = until broken) to the
 * Go-exported handler.
 *
 * `user` is an opaque integer handle, not a pointer: passing a Go pointer to C
 * and letting C hand it back later is exactly what the cgo pointer rules
 * forbid, because the collector may move or free the object in between. The
 * integer indexes a Go-side table instead.
 */
int tf_pcap_loop(pcap_t *handle, int count, uintptr_t user);

/*
 * tf_pcap_open_live wraps pcap_open_live. Its five-argument signature is fine
 * for cgo; the wrapper exists so the Go side has one place to change if we ever
 * move to the pcap_create/pcap_activate pair.
 */
pcap_t *tf_pcap_open_live(const char *device, int snaplen, int promisc,
                          int timeout_ms, char *errbuf);

/*
 * tf_pcap_compile_and_apply compiles a BPF filter expression and installs it,
 * freeing the compiled program either way. Doing both in C keeps the
 * `struct bpf_program` — another cgo-hostile type — out of Go entirely.
 *
 * Returns 0 on success, -1 on failure (call pcap_geterr for the message).
 */
int tf_pcap_compile_and_apply(pcap_t *handle, const char *expr);

/*
 * Below: two functions that do nearly nothing, so that a benchmark can measure
 * the cost of *crossing* into C rather than the cost of the C.
 *
 * The number matters. A CGo call costs on the order of tens of nanoseconds,
 * against roughly one for a Go call — so a C function is worth calling only
 * when it does enough work to dwarf the crossing. It is the single fact that
 * decides whether a CGo binding is an optimization or a pessimization.
 */
int tf_add(int a, int b);

/* Reads n bytes, so a benchmark can compare handing C a copy against handing it
 * a pointer into the Go heap. */
long long tf_sum_bytes(const unsigned char *p, int n);

#endif /* TRACEFORGE_PCAP_SHIM_H */
