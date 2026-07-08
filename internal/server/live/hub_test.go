package live

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"metrics-system/internal/model"
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
