package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"metrics-system/internal/model"
)

func TestIngestAndList(t *testing.T) {
	storage := NewStorage()
	handler := NewHandler(storage, slog.Default())
	routes := handler.Routes()

	batch := model.Batch{
		AgentID: "agent-a",
		Metrics: []model.Metric{{
			Name:      "memory_used_percent",
			Type:      model.MetricTypeGauge,
			Value:     62.4,
			Timestamp: time.Now().UTC(),
		}},
	}

	payload, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	postReq := httptest.NewRequest(http.MethodPost, "/api/v1/metrics", bytes.NewReader(payload))
	postRec := httptest.NewRecorder()
	routes.ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d", http.StatusAccepted, postRec.Code)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)
	getRec := httptest.NewRecorder()
	routes.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, getRec.Code)
	}

	body, err := io.ReadAll(getRec.Body)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}

	var out []model.Metric
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if len(out) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(out))
	}
	if out[0].Name != "memory_used_percent" {
		t.Fatalf("unexpected metric name: %s", out[0].Name)
	}
}

func TestIngestRejectsInvalidJSON(t *testing.T) {
	storage := NewStorage()
	handler := NewHandler(storage, slog.Default())
	routes := handler.Routes()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/metrics", bytes.NewBufferString("{invalid"))
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
	}

	if storage.Count() != 0 {
		t.Fatalf("expected empty storage, got %d entries", storage.Count())
	}
}
