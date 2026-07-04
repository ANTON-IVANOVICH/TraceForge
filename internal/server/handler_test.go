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
	"metrics-system/internal/server/pipeline"
	"metrics-system/internal/server/storage"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestHandler(cfg pipeline.Config, start bool) (*Handler, *pipeline.Pipeline, *storage.MemoryStorage) {
	store := storage.NewMemoryStorage()
	p := pipeline.New(store, cfg, testLogger())
	if start {
		p.Start()
	}
	return NewHandler(p, store, testLogger()), p, store
}

func doReq(h http.Handler, method, target string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, &buf))
	return rec
}

func sampleBatch() model.Batch {
	return model.Batch{
		AgentID: "agent-1",
		Metrics: []model.Metric{{
			Name:      "cpu_usage_percent",
			Type:      model.MetricTypeGauge,
			Value:     42,
			Timestamp: time.Now().UTC(),
		}},
	}
}

func TestHandler_IngestAccepted(t *testing.T) {
	h, _, _ := newTestHandler(pipeline.Config{IngestBuffer: 100}, false)
	rec := doReq(h.Routes(), http.MethodPost, "/api/v1/metrics", sampleBatch())
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
}

func TestHandler_IngestInvalidJSON(t *testing.T) {
	h, _, _ := newTestHandler(pipeline.Config{IngestBuffer: 10}, false)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/metrics", bytes.NewBufferString("{not json")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandler_IngestInvalidBatch(t *testing.T) {
	h, _, _ := newTestHandler(pipeline.Config{IngestBuffer: 10}, false)
	rec := doReq(h.Routes(), http.MethodPost, "/api/v1/metrics", model.Batch{AgentID: "x"}) // no metrics
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandler_IngestRejectsUnknownField(t *testing.T) {
	h, _, _ := newTestHandler(pipeline.Config{IngestBuffer: 10}, false)
	body := `{"agent_id":"a","metrics":[{"name":"m","type":"gauge","value":1,"timestamp":"2026-07-04T10:00:00Z"}],"bogus":1}`
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/metrics", bytes.NewBufferString(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown field must be rejected)", rec.Code)
	}
}

func TestHandler_IngestRejectsTrailingData(t *testing.T) {
	h, _, _ := newTestHandler(pipeline.Config{IngestBuffer: 10}, false)
	body := `{"agent_id":"a","metrics":[{"name":"m","type":"gauge","value":1,"timestamp":"2026-07-04T10:00:00Z"}]}{"x":1}`
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/metrics", bytes.NewBufferString(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (trailing data must be rejected)", rec.Code)
	}
}

func TestHandler_IngestOverloaded(t *testing.T) {
	// Buffer 1, pipeline NOT started => the 2nd ingest overflows -> 503.
	h, _, _ := newTestHandler(pipeline.Config{IngestBuffer: 1}, false)
	if rec := doReq(h.Routes(), http.MethodPost, "/api/v1/metrics", sampleBatch()); rec.Code != http.StatusAccepted {
		t.Fatalf("1st status = %d, want 202", rec.Code)
	}
	rec := doReq(h.Routes(), http.MethodPost, "/api/v1/metrics", sampleBatch())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("2nd status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("503 should carry a Retry-After header")
	}
}

func TestHandler_QueryByName(t *testing.T) {
	h, _, store := newTestHandler(pipeline.Config{IngestBuffer: 10}, false)
	store.Write(model.Metric{Name: "cpu_usage_percent", Type: model.MetricTypeGauge, Value: 7, Timestamp: time.Now().UTC(), Labels: map[string]string{"host": "a"}})

	rec := doReq(h.Routes(), http.MethodGet, "/api/v1/query?name=cpu_usage_percent&host=a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []model.Metric
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Value != 7 {
		t.Fatalf("query result = %+v, want single value 7", got)
	}
}

func TestHandler_QueryMissingName(t *testing.T) {
	h, _, _ := newTestHandler(pipeline.Config{IngestBuffer: 10}, false)
	rec := doReq(h.Routes(), http.MethodGet, "/api/v1/query", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandler_IngestThenQuery(t *testing.T) {
	h, p, _ := newTestHandler(pipeline.Config{IngestBuffer: 100, ValidateWorkers: 2, EnrichWorkers: 2, StoreWorkers: 1}, true)

	if rec := doReq(h.Routes(), http.MethodPost, "/api/v1/metrics", sampleBatch()); rec.Code != http.StatusAccepted {
		t.Fatalf("ingest status = %d, want 202", rec.Code)
	}
	p.Shutdown() // block until the metric flows through all stages into storage

	rec := doReq(h.Routes(), http.MethodGet, "/api/v1/query?name=cpu_usage_percent", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("query status = %d, want 200", rec.Code)
	}
	var got []model.Metric
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 metric after drain, got %d", len(got))
	}
	if got[0].Labels["agent_id"] != "agent-1" {
		t.Errorf("agent_id label = %q, want agent-1 (enrich stage)", got[0].Labels["agent_id"])
	}
}

func TestHandler_Stats(t *testing.T) {
	h, _, _ := newTestHandler(pipeline.Config{IngestBuffer: 10}, false)
	rec := doReq(h.Routes(), http.MethodGet, "/debug/stats", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["pipeline"]; !ok {
		t.Error("stats missing pipeline section")
	}
	if _, ok := body["storage"]; !ok {
		t.Error("stats missing storage section")
	}
}
