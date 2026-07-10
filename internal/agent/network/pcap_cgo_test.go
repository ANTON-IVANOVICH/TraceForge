//go:build cgo

package network

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/testutil"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var captureBase = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// mixedCapture is one savefile with one packet of each kind the collector
// classifies, plus one it cannot.
func mixedCapture(t *testing.T) string {
	t.Helper()
	return writePcap(t, LinkEthernet, 65536, []testPacket{
		{ts: captureBase, data: ethernetFrame(etherTypeIPv4, ipv4Packet(protoTCP))},
		{ts: captureBase.Add(time.Second), data: ethernetFrame(etherTypeIPv4, ipv4Packet(protoUDP))},
		{ts: captureBase.Add(2 * time.Second), data: ethernetFrame(etherTypeIPv4, ipv4Packet(protoICMP))},
		{ts: captureBase.Add(3 * time.Second), data: ethernetFrame(etherTypeIPv6, ipv6Packet(protoTCP, nil))},
		{ts: captureBase.Add(4 * time.Second), data: ethernetFrame(0x0806, make([]byte, 28))}, // ARP
	})
}

func TestOpenOfflineReadsEveryPacket(t *testing.T) {
	path := mixedCapture(t)

	c, err := Open(Config{File: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if got := c.LinkType(); got != LinkEthernet {
		t.Errorf("LinkType: want ethernet, got %v", got)
	}

	var count int
	for {
		pkt, err := c.Next()
		if errors.Is(err, ErrEndOfCapture) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		count++
		if pkt.WireLength != len(pkt.Data) {
			t.Errorf("packet %d: WireLength %d != len(Data) %d", count, pkt.WireLength, len(pkt.Data))
		}
	}
	if count != 5 {
		t.Errorf("read %d packets, want 5", count)
	}
}

func TestNextPreservesTimestamps(t *testing.T) {
	path := writePcap(t, LinkEthernet, 65536, []testPacket{
		{ts: captureBase.Add(1500 * time.Microsecond), data: ethernetFrame(etherTypeIPv4, ipv4Packet(protoTCP))},
	})

	c, err := Open(Config{File: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	pkt, err := c.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	want := captureBase.Add(1500 * time.Microsecond)
	if !pkt.Timestamp.Equal(want) {
		t.Errorf("timestamp: want %s, got %s", want, pkt.Timestamp)
	}
}

// This is the test that pays for the C.GoBytes copy.
//
// libpcap hands back a pointer into a buffer it reuses on the very next call.
// If Next returned a Go slice aliasing that buffer, holding on to packet one
// and then reading packet two would silently rewrite packet one. Delete the
// GoBytes copy in pcap_cgo.go and this test fails; nothing else does.
func TestNextCopiesPacketDataOutOfLibpcapsBuffer(t *testing.T) {
	first := ethernetFrame(etherTypeIPv4, ipv4Packet(protoTCP))
	second := ethernetFrame(etherTypeIPv6, ipv6Packet(protoUDP, nil))

	path := writePcap(t, LinkEthernet, 65536, []testPacket{
		{ts: captureBase, data: first},
		{ts: captureBase.Add(time.Second), data: second},
	})

	c, err := Open(Config{File: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	p1, err := c.Next()
	if err != nil {
		t.Fatalf("Next(1): %v", err)
	}
	kept := p1.Data // deliberately retained across the next call

	if _, err := c.Next(); err != nil {
		t.Fatalf("Next(2): %v", err)
	}

	if !bytes.Equal(kept, first) {
		t.Fatalf("the first packet's bytes changed after reading the second:\n want %x\n  got %x",
			first, kept)
	}
}

// WireLength must report what crossed the wire, not what fitted in the snapshot.
// Counting the truncated copy under-reports throughput by exactly the bytes the
// snapshot length cut off.
func TestNextReportsWireLengthNotCapturedLength(t *testing.T) {
	frame := ethernetFrame(etherTypeIPv4, ipv4Packet(protoTCP))
	path := writePcap(t, LinkEthernet, 64, []testPacket{
		{ts: captureBase, data: frame, wireLength: 1500},
	})

	c, err := Open(Config{File: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	pkt, err := c.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if pkt.WireLength != 1500 {
		t.Errorf("WireLength: want 1500, got %d", pkt.WireLength)
	}
	if len(pkt.Data) != len(frame) {
		t.Errorf("captured bytes: want %d, got %d", len(frame), len(pkt.Data))
	}
}

func TestSetFilterRunsInTheKernel(t *testing.T) {
	path := mixedCapture(t)

	c, err := Open(Config{File: path, Filter: "udp"})
	if err != nil {
		t.Fatalf("Open with filter: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	var seen int
	for {
		pkt, err := c.Next()
		if errors.Is(err, ErrEndOfCapture) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		seen++
		info, ok := Parse(LinkEthernet, pkt.Data)
		if !ok || info.Protocol != protoUDP {
			t.Errorf("a filter of %q let through protocol %d", "udp", info.Protocol)
		}
	}
	if seen != 1 {
		t.Errorf("filter %q matched %d packets, want 1", "udp", seen)
	}
}

func TestSetFilterRejectsAnInvalidExpression(t *testing.T) {
	path := mixedCapture(t)

	_, err := Open(Config{File: path, Filter: "not a bpf expression at all"})
	if err == nil {
		t.Fatal("an invalid filter must fail Open, not be silently ignored")
	}
	if !strings.Contains(err.Error(), "pcap filter") {
		t.Errorf("error should name the filter: %v", err)
	}
}

func TestOpenReportsAMissingFile(t *testing.T) {
	_, err := Open(Config{File: "/nonexistent/capture.pcap"})
	if err == nil {
		t.Fatal("opening a missing savefile must fail")
	}
	if !strings.Contains(err.Error(), "/nonexistent/capture.pcap") {
		t.Errorf("error should name the file: %v", err)
	}
}

func TestCloseIsIdempotentAndPoisonsTheHandle(t *testing.T) {
	path := mixedCapture(t)
	c, err := Open(Config{File: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A double Close on a C handle is a double free. It must be a no-op.
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if _, err := c.Next(); !errors.Is(err, ErrCaptureClosed) {
		t.Errorf("Next after Close: want ErrCaptureClosed, got %v", err)
	}
	if err := c.SetFilter("tcp"); !errors.Is(err, ErrCaptureClosed) {
		t.Errorf("SetFilter after Close: want ErrCaptureClosed, got %v", err)
	}
	if err := c.Loop(1, func(Packet) {}); !errors.Is(err, ErrCaptureClosed) {
		t.Errorf("Loop after Close: want ErrCaptureClosed, got %v", err)
	}
	if _, _, _, err := c.Stats(); !errors.Is(err, ErrCaptureClosed) {
		t.Errorf("Stats after Close: want ErrCaptureClosed, got %v", err)
	}
	c.Break() // must not crash on a freed handle
}

// Close must not strand a reader: whatever a reader is doing, Close returns and
// the reader leaves promptly with a sentinel error.
//
// This is a *liveness* test, and its name once claimed more. It cannot prove the
// safety property — that Close never frees a handle a reader is inside — because
// the failure of that property is a C-level use-after-free, which the race
// detector cannot see, and because a single reader spends nearly all its time
// inside pcap_next_ex where the Go-level race window is vanishingly small.
// Removing the mutex entirely leaves this test passing 20/20.
//
// The safety property is guarded by TestConcurrentCloseDoesNotDoubleFree and
// TestBreakNextAndCloseRacing, which do fail (with a data race and a cgo signal)
// when the mutex is removed.
func TestCloseWhileNextIsRunningReleasesTheReader(t *testing.T) {
	packets := make([]testPacket, 200_000)
	for i := range packets {
		packets[i] = testPacket{
			ts:   captureBase.Add(time.Duration(i) * time.Microsecond),
			data: ethernetFrame(etherTypeIPv4, ipv4Packet(protoTCP)),
		}
	}
	path := writePcap(t, LinkEthernet, 65536, packets)

	c, err := Open(Config{File: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	reading := make(chan struct{})
	readerDone := make(chan error, 1)
	go func() {
		var once sync.Once
		for {
			_, err := c.Next()
			once.Do(func() { close(reading) })
			if err != nil {
				readerDone <- err
				return
			}
		}
	}()

	<-reading // the reader is definitely in the loop, with 200k packets to go
	if err := c.Close(); err != nil {
		t.Fatalf("Close during Next: %v", err)
	}

	select {
	case err := <-readerDone:
		if !errors.Is(err, ErrCaptureClosed) && !errors.Is(err, ErrEndOfCapture) {
			t.Errorf("reader exited with %v, want ErrCaptureClosed or ErrEndOfCapture", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Close did not release the reader")
	}
}

func TestStatsAreUnavailableForASavefile(t *testing.T) {
	path := mixedCapture(t)
	c, err := Open(Config{File: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if _, _, _, err := c.Stats(); err == nil {
		t.Error("a savefile has no kernel drop statistics; Stats must say so")
	}
}

func TestAvailableAndLibraryVersion(t *testing.T) {
	if !Available() {
		t.Error("Available must be true in a CGo build")
	}
	if v := LibraryVersion(); !strings.Contains(strings.ToLower(v), "libpcap") {
		t.Errorf("LibraryVersion: got %q", v)
	}
}

// ---------------------------------------------------------------------------
// C -> Go callbacks
// ---------------------------------------------------------------------------

func TestLoopDeliversEveryPacketToTheCallback(t *testing.T) {
	path := mixedCapture(t)

	c, err := Open(Config{File: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	var got []Packet
	if err := c.Loop(0, func(p Packet) { got = append(got, p) }); err != nil {
		t.Fatalf("Loop: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("callback saw %d packets, want 5", len(got))
	}
	// The same copy rule applies on the callback path: the bytes must survive
	// the loop that produced them.
	if !bytes.Equal(got[0].Data, ethernetFrame(etherTypeIPv4, ipv4Packet(protoTCP))) {
		t.Error("the first packet's bytes did not survive the callback loop")
	}
}

func TestLoopHonoursItsPacketCount(t *testing.T) {
	path := mixedCapture(t)
	c, err := Open(Config{File: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	var n int
	if err := c.Loop(2, func(Packet) { n++ }); err != nil {
		t.Fatalf("Loop: %v", err)
	}
	if n != 2 {
		t.Errorf("Loop(2) delivered %d packets", n)
	}
}

// A panic inside the callback unwinds through C stack frames, which is
// undefined behaviour. It must be caught at the boundary and returned.
func TestLoopTurnsACallbackPanicIntoAnError(t *testing.T) {
	path := mixedCapture(t)
	c, err := Open(Config{File: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	err = c.Loop(0, func(Packet) { panic("callback exploded") })
	if err == nil {
		t.Fatal("a panicking callback must surface as an error from Loop")
	}
	if !strings.Contains(err.Error(), "callback exploded") {
		t.Errorf("error should carry the panic value: %v", err)
	}
}

func TestLoopRejectsANilCallback(t *testing.T) {
	path := mixedCapture(t)
	c, err := Open(Config{File: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if err := c.Loop(0, nil); err == nil {
		t.Error("Loop(nil) must fail rather than hand C a nil Go function")
	}
}

func TestBreakStopsALoopFromAnotherGoroutine(t *testing.T) {
	packets := make([]testPacket, 5000)
	for i := range packets {
		packets[i] = testPacket{
			ts:   captureBase.Add(time.Duration(i) * time.Millisecond),
			data: ethernetFrame(etherTypeIPv4, ipv4Packet(protoTCP)),
		}
	}
	path := writePcap(t, LinkEthernet, 65536, packets)

	c, err := Open(Config{File: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	seen := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.Loop(0, func(Packet) {
			select {
			case seen <- struct{}{}:
			default:
			}
		})
	}()

	<-seen // the loop is definitely inside libpcap now
	c.Break()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Loop after Break: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Break did not stop the loop")
	}
}

// ---------------------------------------------------------------------------
// The collector
// ---------------------------------------------------------------------------

func TestCollectorCountsPacketsFromASavefile(t *testing.T) {
	defer testutil.NoLeaks(t)()

	path := mixedCapture(t)
	c, err := NewCollector(Config{File: path}, discardLogger())
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Run returns at end of savefile.
	c.Run(context.Background())

	metrics, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	byKey := map[string]float64{}
	for _, m := range metrics {
		key := m.Name
		if p, ok := m.Labels["protocol"]; ok {
			key += "/" + p
		}
		if v, ok := m.Labels["version"]; ok {
			key += "/v" + v
		}
		byKey[key] = m.Value
		if m.Type != model.MetricTypeCounter {
			t.Errorf("%s must be a counter", m.Name)
		}
		if m.Labels["device"] != path {
			t.Errorf("%s: device label = %q, want %q", m.Name, m.Labels["device"], path)
		}
	}

	want := map[string]float64{
		"net_packets_total":                5,
		"net_protocol_packets_total/tcp":   2, // one IPv4, one IPv6
		"net_protocol_packets_total/udp":   1,
		"net_protocol_packets_total/icmp":  1,
		"net_protocol_packets_total/other": 0,
		"net_ip_packets_total/v4":          3,
		"net_ip_packets_total/v6":          1,
		"net_unparsed_packets_total":       1, // the ARP frame
		"net_read_errors_total":            0,
	}
	for key, expected := range want {
		if got, ok := byKey[key]; !ok {
			t.Errorf("missing metric %s", key)
		} else if got != expected {
			t.Errorf("%s = %v, want %v", key, got, expected)
		}
	}

	// A savefile has no kernel statistics, so those metrics must be absent
	// rather than reported as zero — a fabricated zero drop count is worse than
	// no drop count.
	if _, ok := byKey["net_kernel_dropped_total"]; ok {
		t.Error("net_kernel_dropped_total must not be emitted for a savefile")
	}
}

func TestCollectorRunStopsOnContextCancel(t *testing.T) {
	defer testutil.NoLeaks(t)()

	packets := make([]testPacket, 20000)
	for i := range packets {
		packets[i] = testPacket{ts: captureBase, data: ethernetFrame(etherTypeIPv4, ipv4Packet(protoTCP))}
	}
	path := writePcap(t, LinkEthernet, 65536, packets)

	c, err := NewCollector(Config{File: path}, discardLogger())
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	testutil.Eventually(t, 5*time.Second, time.Millisecond, func() bool {
		return c.packets.Load() > 0
	}, "the collector should start counting packets")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

func TestCollectorCloseWithoutRunDoesNotBlock(t *testing.T) {
	path := mixedCapture(t)
	c, err := NewCollector(Config{File: path}, discardLogger())
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Close waited %s for a Run that never started", elapsed)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// Close() calls Break() (which takes the read lock) and only then mu.Lock().
// A Go RWMutex blocks new readers once a writer is waiting, so ordering these
// the other way round would have Break wait behind the very writer it exists to
// unblock — a deadlock that only appears when a Loop is actually running.
func TestCloseDuringLoopDoesNotDeadlock(t *testing.T) {
	packets := make([]testPacket, 200_000)
	for i := range packets {
		packets[i] = testPacket{ts: captureBase, data: ethernetFrame(etherTypeIPv4, ipv4Packet(protoTCP))}
	}
	path := writePcap(t, LinkEthernet, 65536, packets)

	c, err := Open(Config{File: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	inLoop := make(chan struct{}, 1)
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		_ = c.Loop(0, func(Packet) {
			select {
			case inLoop <- struct{}{}:
			default:
			}
		})
	}()
	<-inLoop // the loop is definitely inside libpcap

	closed := make(chan struct{})
	go func() { _ = c.Close(); close(closed) }()

	select {
	case <-closed:
	case <-time.After(15 * time.Second):
		t.Fatal("Close deadlocked against a running Loop")
	}
	<-loopDone
}

// pcap_close frees the handle. Two goroutines closing at once must not free it
// twice, and the AddCleanup safety net must not free it a third time later.
func TestConcurrentCloseDoesNotDoubleFree(t *testing.T) {
	path := mixedCapture(t)
	c, err := Open(Config{File: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	wg.Wait()
}

// The collector's watcher goroutine calls Break on cancellation while Close is
// also calling Break and then freeing. Run under -race.
func TestBreakNextAndCloseRacing(t *testing.T) {
	path := mixedCapture(t)

	for i := 0; i < 50; i++ {
		c, err := Open(Config{File: path})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); c.Break() }()
		go func() { defer wg.Done(); _, _ = c.Next() }()
		go func() { defer wg.Done(); _ = c.Close() }()
		wg.Wait()
	}
}

// A panicking callback must *stop* the loop, not merely record an error.
//
// Recording an error and returning leaves pcap_loop dispatching. Every later
// packet takes the handler's cheap early-out, so nothing is delivered and a
// naive "count the callbacks" assertion sees exactly one either way — this test
// was written that way first, and passed with the fix reverted. On a savefile
// the loop still ends and the bug hides; on a live interface it does not, and
// Loop hangs forever holding the read lock that Close needs.
//
// The observable is therefore time, not deliveries: with pcap_breakloop, Loop
// abandons the file at once; without it, it walks every packet in C.
func TestLoopStopsImmediatelyWhenTheCallbackPanics(t *testing.T) {
	const packets = 400_000
	frames := make([]testPacket, packets)
	for i := range frames {
		frames[i] = testPacket{ts: captureBase, data: ethernetFrame(etherTypeIPv4, ipv4Packet(protoTCP))}
	}
	path := writePcap(t, LinkEthernet, 65536, frames)

	// Baseline: how long libpcap needs to walk the whole savefile.
	full, err := Open(Config{File: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	start := time.Now()
	if err := full.Loop(0, func(Packet) {}); err != nil {
		t.Fatalf("baseline Loop: %v", err)
	}
	fullTraversal := time.Since(start)
	_ = full.Close()

	// Now panic on the first packet.
	c, err := Open(Config{File: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	start = time.Now()
	err = c.Loop(0, func(Packet) { panic("callback exploded on the first packet") })
	panicked := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "callback exploded") {
		t.Fatalf("Loop should surface the panic, got %v", err)
	}
	// A loop that was actually broken returns in microseconds. One that merely
	// stopped delivering still pays for every packet in the file; it comes in a
	// little under the baseline (no copies), never near zero.
	if limit := fullTraversal / 10; panicked > limit {
		t.Errorf("Loop took %s after the panic (a full traversal is %s): pcap_breakloop was not called",
			panicked, fullTraversal)
	}
}
