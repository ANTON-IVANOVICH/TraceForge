package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"metrics-system/internal/server/ratelimit"
)

func TestRecover(t *testing.T) {
	h := Recover(testLogger())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (panic must not crash the server)", rec.Code)
	}
}

func TestRequestID_SetsHeaderAndContext(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Error("request id missing from context")
	}
	if rec.Header().Get("X-Request-ID") != seen {
		t.Errorf("header %q != context %q", rec.Header().Get("X-Request-ID"), seen)
	}
}

func TestRequestID_PreservesIncoming(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "abc")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-ID") != "abc" {
		t.Errorf("incoming request id not preserved: %q", rec.Header().Get("X-Request-ID"))
	}
}

func TestChain_Order(t *testing.T) {
	var order []string
	mw := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := Chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { order = append(order, "handler") }),
		mw("outer"), mw("inner"),
	)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"outer", "inner", "handler"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestRateLimit_Middleware(t *testing.T) {
	lim := ratelimit.New(0, 1) // one token, no refill
	h := RateLimit(lim)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("X-Agent-ID", "a1")
		return r
	}

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req())
	if rec1.Code != http.StatusOK {
		t.Errorf("first = %d, want 200", rec1.Code)
	}
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req())
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("second = %d, want 429", rec2.Code)
	}
}

func TestStatusRecorder(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}
	sr.WriteHeader(http.StatusTeapot)
	n, _ := sr.Write([]byte("hello"))
	if sr.status != http.StatusTeapot {
		t.Errorf("status = %d, want 418", sr.status)
	}
	if sr.written != 5 || n != 5 {
		t.Errorf("written = %d / n = %d, want 5/5", sr.written, n)
	}
}
