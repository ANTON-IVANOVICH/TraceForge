package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"metrics-system/internal/model"
	"metrics-system/pkg/httpx"
)

func TestSenderSend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected method POST, got %s", r.Method)
		}

		var batch model.Batch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Fatalf("decode failed: %v", err)
		}

		if batch.AgentID != "test-agent" {
			t.Fatalf("expected agent id test-agent, got %s", batch.AgentID)
		}

		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	sender := NewSender(srv.URL, httpx.NewClient(2*time.Second, 0, 50*time.Millisecond))
	batch := model.Batch{
		AgentID: "test-agent",
		Metrics: []model.Metric{{
			Name:      "cpu_usage_percent",
			Type:      model.MetricTypeGauge,
			Value:     50,
			Timestamp: time.Now().UTC(),
		}},
	}

	if err := sender.Send(context.Background(), batch); err != nil {
		t.Fatalf("send failed: %v", err)
	}
}

func TestSenderRetriesOnServerError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&calls, 1)
		if count == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	sender := NewSender(srv.URL, httpx.NewClient(2*time.Second, 2, 10*time.Millisecond))
	batch := model.Batch{
		AgentID: "retry-agent",
		Metrics: []model.Metric{{
			Name:      "uptime_seconds",
			Type:      model.MetricTypeCounter,
			Value:     123,
			Timestamp: time.Now().UTC(),
		}},
	}

	if err := sender.Send(context.Background(), batch); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Fatalf("expected at least 2 calls, got %d", got)
	}
}
