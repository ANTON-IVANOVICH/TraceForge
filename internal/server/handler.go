package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"net/url"
	"strconv"
	"time"

	"metrics-system/internal/model"
	"metrics-system/internal/server/pipeline"
	"metrics-system/internal/server/storage"
)

// Handler is a thin HTTP layer: ingestion feeds the pipeline, reads go straight
// to storage. It carries no business logic itself.
type Handler struct {
	pipeline *pipeline.Pipeline
	storage  storage.Storage
	logger   *slog.Logger
}

// NewHandler wires the handler to its pipeline and storage.
func NewHandler(p *pipeline.Pipeline, store storage.Storage, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{pipeline: p, storage: store, logger: logger}
}

// Routes builds the mux with the API, health, self-stats and pprof endpoints.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/metrics", h.ingest)
	mux.HandleFunc("GET /api/v1/query", h.query)
	mux.HandleFunc("GET /debug/stats", h.stats)
	mux.HandleFunc("GET /healthz", h.health)

	// Runtime profiling (net/http/pprof).
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	return mux
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
