package live

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/testutil"
)

func discardHub() *Hub { return NewHub(slog.New(slog.NewTextHandler(io.Discard, nil))) }

// wsClient is the browser side of an in-memory WebSocket pipe.
type wsClient struct {
	conn net.Conn
	r    *bufio.Reader
}

// wsPair connects a client-side reader to a server-side Conn via net.Pipe.
func wsPair() (*wsClient, *Conn) {
	c1, c2 := net.Pipe()
	return &wsClient{conn: c1, r: bufio.NewReader(c1)}, serverConn(c2)
}

// readText reads the next text frame, guarded by a deadline so a missing
// broadcast fails fast instead of hanging.
func (c *wsClient) readText(t *testing.T) string {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	op, payload := readServerFrame(t, c.r)
	if op != opText {
		t.Fatalf("expected text frame, got opcode %x", op)
	}
	return string(payload)
}

func metric(name, tenant string) model.Metric {
	return model.Metric{
		Name:      name,
		Type:      model.MetricTypeGauge,
		Value:     1,
		Timestamp: time.Unix(1783000000, 0).UTC(),
		Labels:    map[string]string{"tenant": tenant},
	}
}

func TestHubBroadcastTenantFilter(t *testing.T) {
	t.Parallel()
	hub := discardHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)

	a, aConn := wsPair()
	b, bConn := wsPair()
	hub.Add(aConn, "tenant-a", true) // Add blocks until Run has registered it
	hub.Add(bConn, "tenant-b", false)

	hub.PublishMetrics([]model.Metric{metric("cpu", "tenant-a"), metric("mem", "tenant-b")})

	if msg := a.readText(t); !strings.Contains(msg, "cpu") || strings.Contains(msg, "mem") {
		t.Fatalf("tenant-a got %q, want only cpu", msg)
	}
	if msg := b.readText(t); !strings.Contains(msg, "mem") || strings.Contains(msg, "cpu") {
		t.Fatalf("tenant-b got %q, want only mem", msg)
	}
}

// TestHubConcurrentSubscribeUnregister is the classic hub-leak trap: a goroutine
// stranded per client that connected and left. 100 goroutines each register,
// receive a publish, then close their socket (the only way to unregister — a
// dropped connection cascades conn error -> readPump -> drop -> Run deletes the
// client and closes its send, which stops writePump). NoLeaks then proves no
// pump or connection goroutine outlived its client.
//
// Not parallel: NoLeaks compares the goroutine set before and after, so it must
// run in the sequential phase where no sibling test is spawning goroutines.
func TestHubConcurrentSubscribeUnregister(t *testing.T) {
	defer testutil.NoLeaks(t)()

	hub := discardHub()
	ctx, cancel := context.WithCancel(context.Background())
	go hub.Run(ctx)

	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tenant := fmt.Sprintf("tenant-%d", i%4)
			cl, sc := wsPair()
			hub.Add(sc, tenant, i%2 == 0) // blocks until Run registers it
			hub.PublishMetrics([]model.Metric{metric("cpu", tenant)})
			_ = cl.conn.Close() // leaving: drops the client and must reap its goroutines
		}(i)
	}
	wg.Wait()

	cancel() // stop Run; NoLeaks (deferred, retries ~1s) then confirms the teardown
}

// TestHubSlowClientDoesNotBlockPublish guards an availability property: one
// dashboard that never reads its socket must not stall delivery to everyone else.
// The hub's deliver is a non-blocking send, so a slow client only misses updates.
// If it could ever block the Run goroutine, the healthy client below would never
// receive its frame and readText's deadline would fire.
func TestHubSlowClientDoesNotBlockPublish(t *testing.T) {
	hub := discardHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)

	// Slow client: never drained. Its writePump blocks on the first pipe write and
	// its 64-deep send buffer then fills; further deliveries are dropped.
	_, slowConn := wsPair()
	hub.Add(slowConn, "", false)

	fast, fastConn := wsPair()
	hub.Add(fastConn, "", false)

	// Overrun the slow client's buffer many times over.
	for i := 0; i < 500; i++ {
		hub.PublishMetrics([]model.Metric{metric("cpu", "")})
	}

	if msg := fast.readText(t); !strings.Contains(msg, "cpu") {
		t.Fatalf("fast client got %q, want a metrics frame — the hub stalled on the slow client", msg)
	}
}

func TestHubStatsAdminOnly(t *testing.T) {
	t.Parallel()
	hub := discardHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)

	admin, adminConn := wsPair()
	hub.Add(adminConn, "", true)
	hub.PublishStats(map[string]any{"pipeline": map[string]int{"stored": 7}})
	if msg := admin.readText(t); !strings.Contains(msg, `"stats"`) || !strings.Contains(msg, "7") {
		t.Fatalf("admin stats payload = %q", msg)
	}

	// A non-admin client must NOT receive stats. Publish stats then a metric;
	// the first (and only) frame it sees must be the metric.
	na, naConn := wsPair()
	hub.Add(naConn, "tenant-a", false)
	hub.PublishStats(map[string]any{"pipeline": map[string]int{"stored": 99}})
	hub.PublishMetrics([]model.Metric{metric("cpu", "tenant-a")})

	msg := na.readText(t)
	if strings.Contains(msg, "stats") || strings.Contains(msg, "99") {
		t.Fatalf("non-admin received stats: %q", msg)
	}
	if !strings.Contains(msg, "metrics") || !strings.Contains(msg, "cpu") {
		t.Fatalf("non-admin first frame = %q, want the metrics event", msg)
	}
}
