package live

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"metrics-system/internal/model"
)

// pingPeriod must be shorter than pongWait so an idle connection is proven live
// before it is considered stale.
const pingPeriod = 50 * time.Second

// MetricDTO is the wire shape of a metric pushed to the dashboard.
type MetricDTO struct {
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	Value     float64           `json:"value"`
	Timestamp time.Time         `json:"timestamp"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// AlertEvent is the wire shape of an alert pushed to the dashboard. The live
// package deliberately does not import the alerting packages — it is a
// transport, so the caller adapts its domain type into this one.
type AlertEvent struct {
	Fingerprint string            `json:"fingerprint"`
	Rule        string            `json:"rule"`
	Status      string            `json:"status"` // "firing" | "resolved"
	Severity    string            `json:"severity"`
	Value       float64           `json:"value"`
	StartsAt    time.Time         `json:"starts_at"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type envelope struct {
	Type    string      `json:"type"` // "metrics" | "stats" | "alert"
	Metrics []MetricDTO `json:"metrics,omitempty"`
	Stats   any         `json:"stats,omitempty"`
	Alert   *AlertEvent `json:"alert,omitempty"`
}

// client is one connected dashboard. tenant scopes which metrics it may see
// (empty = unrestricted: auth off or an admin); admin gates the stats stream.
type client struct {
	conn   *Conn
	tenant string
	admin  bool
	send   chan []byte
}

// Hub fans live events out to connected dashboards. A single goroutine (Run)
// owns the client set, so there are no locks around it; all mutation flows
// through channels.
type Hub struct {
	registerCh   chan *client
	unregisterCh chan *client
	metricsCh    chan []model.Metric
	statsCh      chan []byte
	alertCh      chan AlertEvent
	clients      map[*client]struct{}
	done         chan struct{} // closed when Run exits
	logger       *slog.Logger
}

// NewHub creates a hub. Call Run (once) to start it.
func NewHub(logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		registerCh:   make(chan *client),
		unregisterCh: make(chan *client, 16),
		metricsCh:    make(chan []model.Metric, 64),
		statsCh:      make(chan []byte, 8),
		alertCh:      make(chan AlertEvent, 64),
		clients:      make(map[*client]struct{}),
		done:         make(chan struct{}),
		logger:       logger,
	}
}

// Add registers a freshly-upgraded connection with the given tenant scope. If
// the hub has already stopped (shutdown), it closes the connection instead of
// blocking forever on the unbuffered registerCh.
func (h *Hub) Add(conn *Conn, tenant string, admin bool) {
	c := &client{conn: conn, tenant: tenant, admin: admin, send: make(chan []byte, 64)}
	select {
	case h.registerCh <- c:
	case <-h.done:
		_ = conn.Close()
	}
}

// PublishMetrics offers newly-stored metrics to the hub. It never blocks the
// caller (the pipeline): it copies the slice and drops the event if the hub is
// saturated.
func (h *Hub) PublishMetrics(ms []model.Metric) {
	if len(ms) == 0 {
		return
	}
	cp := make([]model.Metric, len(ms))
	copy(cp, ms)
	select {
	case h.metricsCh <- cp:
	default: // hub busy; drop this batch from the live feed
	}
}

// PublishAlert offers an alert to the hub. Like PublishMetrics it never blocks
// the caller (here, the notifier's forwarding goroutine).
func (h *Hub) PublishAlert(ev AlertEvent) {
	select {
	case h.alertCh <- ev:
	default: // hub busy; the alert still reaches its receivers, just not the UI
	}
}

// PublishStats offers a stats snapshot (delivered only to admin clients).
func (h *Hub) PublishStats(v any) {
	data, err := json.Marshal(envelope{Type: "stats", Stats: v})
	if err != nil {
		return
	}
	select {
	case h.statsCh <- data:
	default:
	}
}

// Run owns the client set until ctx is cancelled.
func (h *Hub) Run(ctx context.Context) {
	defer close(h.done)
	for {
		select {
		case <-ctx.Done():
			for c := range h.clients {
				close(c.send)
				go func() { _ = c.conn.Close() }() // off the hub goroutine: Close may block on a stalled socket
			}
			h.clients = map[*client]struct{}{}
			return
		case c := <-h.registerCh:
			h.clients[c] = struct{}{}
			go h.writePump(c)
			go h.readPump(c)
		case c := <-h.unregisterCh:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
				go func() { _ = c.conn.Close() }() // off the hub goroutine: Close may block on a stalled socket
			}
		case ms := <-h.metricsCh:
			h.broadcastMetrics(ms)
		case ev := <-h.alertCh:
			h.broadcastAlert(ev)
		case data := <-h.statsCh:
			for c := range h.clients {
				if c.admin {
					deliver(c, data)
				}
			}
		}
	}
}

func (h *Hub) broadcastMetrics(ms []model.Metric) {
	for c := range h.clients {
		dto := filterForTenant(ms, c.tenant)
		if len(dto) == 0 {
			continue
		}
		data, err := json.Marshal(envelope{Type: "metrics", Metrics: dto})
		if err != nil {
			continue
		}
		deliver(c, data)
	}
}

// broadcastAlert sends the alert only to clients allowed to see its tenant.
func (h *Hub) broadcastAlert(ev AlertEvent) {
	data, err := json.Marshal(envelope{Type: "alert", Alert: &ev})
	if err != nil {
		return
	}
	for c := range h.clients {
		if c.tenant != "" && ev.Labels["tenant"] != c.tenant {
			continue
		}
		deliver(c, data)
	}
}

// deliver enqueues a message without blocking the hub; a slow client that has
// filled its buffer simply misses this update.
func deliver(c *client, data []byte) {
	select {
	case c.send <- data:
	default:
	}
}

// filterForTenant maps metrics to DTOs, keeping only those visible to a client
// scoped to tenant (empty tenant sees everything).
func filterForTenant(ms []model.Metric, tenant string) []MetricDTO {
	out := make([]MetricDTO, 0, len(ms))
	for _, m := range ms {
		if tenant != "" && m.Labels["tenant"] != tenant {
			continue
		}
		out = append(out, MetricDTO{
			Name:      m.Name,
			Type:      m.Type.String(),
			Value:     m.Value,
			Timestamp: m.Timestamp,
			Labels:    m.Labels,
		})
	}
	return out
}

func (h *Hub) writePump(c *client) {
	ping := time.NewTicker(pingPeriod)
	defer ping.Stop()
	for {
		select {
		case msg, ok := <-c.send:
			if !ok { // hub closed the channel on unregister
				return
			}
			if err := c.conn.WriteText(msg); err != nil {
				h.drop(c)
				return
			}
		case <-ping.C:
			if err := c.conn.Ping(); err != nil {
				h.drop(c)
				return
			}
		}
	}
}

func (h *Hub) readPump(c *client) {
	_ = c.conn.ReadLoop()
	h.drop(c)
}

// drop asks Run to unregister a client, but gives up if Run has already exited
// (shutdown) so the pump goroutine can't leak blocking on the channel.
func (h *Hub) drop(c *client) {
	select {
	case h.unregisterCh <- c:
	case <-h.done:
	}
}
