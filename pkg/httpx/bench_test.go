package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

var (
	sinkResp *http.Response
	sinkErr  error
	sinkBool bool
)

// BenchmarkShouldRetry times the pure retry decision in isolation — no clock, no
// socket — so its cost is unambiguous and comparable across the three verdicts a
// response can produce.
func BenchmarkShouldRetry(b *testing.B) {
	okResp := &http.Response{StatusCode: http.StatusOK}
	errResp := &http.Response{StatusCode: http.StatusServiceUnavailable}
	transportErr := errors.New("dial tcp: connection refused")

	cases := []struct {
		name string
		resp *http.Response
		err  error
	}{
		{"success", okResp, nil},
		{"server-error", errResp, nil},
		{"transport-error", nil, transportErr},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sinkBool = shouldRetry(c.resp, c.err)
			}
		})
	}
}

// BenchmarkClientDo measures a full round-trip through Do against a local server.
// The one-retry server fails the first call of every pair and succeeds the
// second; retries is 1 and backoff is a single nanosecond, so the retry branch
// exercises the timer/select control flow without ever timing a real sleep — the
// number reflects the retry machinery, not the wait.
func BenchmarkClientDo(b *testing.B) {
	b.Run("success", func(b *testing.B) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := NewClient(2*time.Second, 2, time.Nanosecond)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
			if err != nil {
				b.Fatalf("new request: %v", err)
			}
			resp, err := c.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			sinkResp, sinkErr = resp, err
		}
	})

	b.Run("one-retry", func(b *testing.B) {
		var n int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&n, 1)%2 == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := NewClient(2*time.Second, 1, time.Nanosecond)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
			if err != nil {
				b.Fatalf("new request: %v", err)
			}
			resp, err := c.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			sinkResp, sinkErr = resp, err
		}
	})
}
