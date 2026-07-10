package promexport

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	c := NewCounterVec("http_requests_total", "requests handled", []string{"code"}, 10)
	c.WithLabelValues("200").Add(7)

	h := Handler(nil, c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != contentType {
		t.Errorf("Content-Type = %q, want %q", ct, contentType)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	// The body must parse, and the parsed sample must carry the observed value —
	// proving the handler serves a real, decodable scrape rather than a 200 with
	// an empty body.
	samples := parseExposition(t, string(body))
	var found bool
	for _, s := range samples {
		if s.name == "http_requests_total" && s.labels["code"] == "200" {
			found = true
			if s.value != 7 {
				t.Errorf("http_requests_total{code=200} = %v, want 7", s.value)
			}
		}
	}
	if !found {
		t.Errorf("http_requests_total{code=200} not found in body:\n%s", body)
	}
}

// TestHandlerDeduplicatesFamilyName is the real-world shape of the cross-family
// duplicate-name bug: two independently-registered gatherers both emit a family
// with the same name. The scrape must stay parseable — one HELP line for the
// name, not two — and the endpoint must still answer 200, since dropping one
// family is a warning, not a scrape failure.
func TestHandlerDeduplicatesFamilyName(t *testing.T) {
	a := GathererFunc(func() []Family {
		return []Family{{Name: "shared_requests_total", Help: "shared", Type: TypeCounter, Samples: []Sample{{Value: 1}}}}
	})
	b := GathererFunc(func() []Family {
		return []Family{{Name: "shared_requests_total", Help: "shared", Type: TypeCounter, Samples: []Sample{{Value: 2}}}}
	})

	h := Handler(nil, a, b)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if n := strings.Count(string(body), "# HELP shared_requests_total "); n != 1 {
		t.Errorf("shared_requests_total emitted %d HELP lines, want exactly 1:\n%s", n, body)
	}
}

// TestHandlerServesValidDespiteBadFamily proves the handler still serves the
// good metrics when a gatherer hands it an invalid family, rather than failing
// the whole scrape.
func TestHandlerServesValidDespiteBadFamily(t *testing.T) {
	bad := GathererFunc(func() []Family {
		return []Family{{Name: "1invalid", Type: TypeCounter, Samples: []Sample{{Value: 1}}}}
	})
	good := GathererFunc(func() []Family {
		return []Family{{Name: "ok_total", Type: TypeCounter, Samples: []Sample{{Value: 3}}}}
	})

	h := Handler(nil, bad, good)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	samples := parseExposition(t, string(body))
	if len(samples) != 1 || samples[0].name != "ok_total" || samples[0].value != 3 {
		t.Errorf("expected only ok_total=3, got %+v\nbody:\n%s", samples, body)
	}
}
