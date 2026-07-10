package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"metrics-system/internal/alerting"
	"metrics-system/internal/auth"
	"metrics-system/internal/model"
	"metrics-system/internal/server/live"
	"metrics-system/internal/server/pipeline"
	"metrics-system/internal/server/storage"
	"metrics-system/web"
)

// Handler is a thin HTTP layer: ingestion feeds the pipeline, reads go straight
// to storage. It carries no business logic itself.
type Handler struct {
	pipeline *pipeline.Pipeline
	storage  storage.Storage
	logger   *slog.Logger

	// UI (optional): set via SetUI. When hub is non-nil the dashboard and its
	// WebSocket are served.
	hub   *live.Hub
	authn auth.Authenticator

	// Alerting (optional): set via SetAlerting. When non-nil the rules, alerts
	// and silences API is served.
	alerting *alerting.Service
}

// NewHandler wires the handler to its pipeline and storage.
func NewHandler(p *pipeline.Pipeline, store storage.Storage, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{pipeline: p, storage: store, logger: logger}
}

// SetUI enables the embedded dashboard. authn (may be nil = auth off) is used to
// authenticate and tenant-scope the WebSocket stream. Call before Routes.
func (h *Handler) SetUI(hub *live.Hub, authn auth.Authenticator) {
	h.hub = hub
	h.authn = authn
}

// SetAlerting enables the alerting API. Call before Routes.
func (h *Handler) SetAlerting(svc *alerting.Service) { h.alerting = svc }

// Routes builds the mux with the API, health and self-stats endpoints.
//
// pprof is deliberately absent: it lives on its own listener, off by default.
// See NewProfilingServer.
// It returns the concrete *http.ServeMux rather than an http.Handler because the
// metrics middleware needs to ask it which pattern a request matched, and only a
// mux can answer that.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/metrics", h.ingest)
	mux.HandleFunc("GET /api/v1/query", h.query)
	mux.HandleFunc("GET /debug/stats", h.stats)
	mux.HandleFunc("GET /healthz", h.health)

	// Rules, alerts and silences (only when alerting is enabled via SetAlerting).
	if h.alerting != nil {
		h.alertRoutes(mux)
	}

	// Embedded dashboard + live WebSocket (only when enabled via SetUI).
	if h.hub != nil {
		sub, _ := fs.Sub(web.FS, "static")
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
		mux.HandleFunc("GET /{$}", h.index)
		mux.HandleFunc("GET /ws", h.ws)
	}
	return mux
}

// index serves the dashboard shell.
func (h *Handler) index(w http.ResponseWriter, _ *http.Request) {
	data, err := web.FS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "ui unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// ws authenticates (when auth is on) and upgrades to a WebSocket, registering
// the connection with the live hub scoped to the caller's tenant.
func (h *Handler) ws(w http.ResponseWriter, r *http.Request) {
	tenant, admin, ok := h.authorizeWS(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	conn, err := live.Upgrade(w, r)
	if err != nil {
		h.logger.Debug("ws upgrade failed", "error", err)
		return
	}
	h.hub.Add(conn, tenant, admin)
}

// authorizeWS resolves the WebSocket caller. Browsers can't set custom headers
// on the WS handshake, so credentials come from query params (?token / ?api_key).
// Auth off => unrestricted view (may see stats).
func (h *Handler) authorizeWS(r *http.Request) (tenant string, admin, ok bool) {
	if h.authn == nil {
		return "", true, true
	}
	q := r.URL.Query()
	principal, err := h.authn.Authenticate(r.Context(), auth.Credentials{
		APIKey: q.Get("api_key"),
		Bearer: q.Get("token"),
	})
	if err != nil || !principal.Can(auth.ActionQuery) {
		return "", false, false
	}
	return principal.Tenant, principal.Can(auth.ActionAdmin), true
}

func (h *Handler) ingest(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var batch model.Batch
	if err := dec.Decode(&batch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	// Reject any trailing data after the batch object.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := batch.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Stamp the server-side tenant from the authenticated principal (if any),
	// so the pipeline can isolate this data. No principal => single-tenant.
	if p, ok := auth.FromContext(r.Context()); ok {
		batch.Tenant = p.Tenant
	}
	if !h.pipeline.Ingest(batch) {
		// Pipeline saturated: tell the client to back off.
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "pipeline overloaded")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) query(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Enforce tenant isolation: a tenant-scoped principal may only read its own
	// series, overriding any client-supplied tenant filter.
	if p, ok := auth.FromContext(r.Context()); ok && p.Tenant != "" {
		if q.Labels == nil {
			q.Labels = make(map[string]string, 1)
		}
		q.Labels["tenant"] = p.Tenant
	}
	result, err := h.storage.Query(q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if result == nil {
		result = []model.Metric{}
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) stats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"pipeline": h.pipeline.Stats(),
		"storage":  h.storage.Stats(),
	})
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// reservedQueryParams are the query keys with dedicated meaning; everything
// else is treated as a label filter.
var reservedQueryParams = map[string]bool{
	"name": true, "from": true, "to": true, "agg": true, "step": true, "limit": true,
}

// parseQuery builds a storage.Query from URL params, e.g.
// /api/v1/query?name=cpu_usage_percent&host=web-1&from=2026-01-01T00:00:00Z&agg=avg&step=1m
func parseQuery(v url.Values) (storage.Query, error) {
	q := storage.Query{Name: v.Get("name")}
	if q.Name == "" {
		return q, errors.New("name is required")
	}

	labels := make(map[string]string)
	for k, vals := range v {
		if !reservedQueryParams[k] && len(vals) > 0 {
			labels[k] = vals[0]
		}
	}
	if len(labels) > 0 {
		q.Labels = labels
	}

	if s := v.Get("from"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return q, fmt.Errorf("from: %w", err)
		}
		q.From = t
	}
	if s := v.Get("to"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return q, fmt.Errorf("to: %w", err)
		}
		q.To = t
	}
	if s := v.Get("step"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return q, fmt.Errorf("step: %w", err)
		}
		q.Step = d
	}
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return q, errors.New("limit: must be a non-negative integer")
		}
		q.Limit = n
	}

	agg, err := storage.AggregatorByName(v.Get("agg"))
	if err != nil {
		return q, err
	}
	q.Aggregator = agg
	return q, nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
