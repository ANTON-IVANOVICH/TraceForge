package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"metrics-system/internal/model"
)

type Handler struct {
	storage *Storage
	logger  *slog.Logger
}

func NewHandler(storage *Storage, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{storage: storage, logger: logger}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/metrics", h.ingest)
	mux.HandleFunc("GET /api/v1/metrics", h.list)
	mux.HandleFunc("GET /healthz", h.health)
	return mux
}

func (h *Handler) ingest(w http.ResponseWriter, r *http.Request) {
	defer func() {
		_ = r.Body.Close()
	}()
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var batch model.Batch
	if err := dec.Decode(&batch); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if err := batch.Validate(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	h.storage.Add(batch)
	h.logger.Debug("ingested", "agent", batch.AgentID, "count", len(batch.Metrics))
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) list(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(h.storage.All()); err != nil {
		h.logger.Error("encode failed", "error", err)
	}
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
