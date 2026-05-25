package httpx

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientRetries5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	client := NewClient(2*time.Second, 2, 10*time.Millisecond)
	req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader([]byte(`{"x":1}`)))
	if err != nil {
		t.Fatalf("new request failed: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do failed: %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected %d, got %d", http.StatusAccepted, resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Fatalf("expected >= 2 calls, got %d", got)
	}
}
