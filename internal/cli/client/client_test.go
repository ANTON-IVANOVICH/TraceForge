package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"metrics-system/internal/cli/config"
)

func newTestClient(t *testing.T, server string, auth config.Auth) *Client {
	t.Helper()
	c, err := New(&config.Context{Server: server, Auth: auth}, 5*time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewValidatesTheServerURL(t *testing.T) {
	t.Parallel()
	for name, server := range map[string]string{
		"empty":      "",
		"no scheme":  "localhost:8080",
		"bad scheme": "ftp://example.com",
		"no host":    "http://",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(&config.Context{Server: server}, 0); err == nil {
				t.Fatalf("server %q was accepted", server)
			}
		})
	}
	if _, err := New(&config.Context{Server: "https://example.com/"}, 0); err != nil {
		t.Fatalf("a valid server was rejected: %v", err)
	}
}

func TestCredentialsAreSent(t *testing.T) {
	t.Parallel()
	var gotKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, config.Auth{APIKey: "k1"})
	if _, err := c.ListRules(context.Background()); err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if gotKey != "k1" || gotAuth != "" {
		t.Fatalf("api key: X-API-Key=%q Authorization=%q", gotKey, gotAuth)
	}

	c = newTestClient(t, srv.URL, config.Auth{Token: "t1"})
	if _, err := c.ListRules(context.Background()); err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if gotAuth != "Bearer t1" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
}

// A token-file is read at construction, so rotating the file needs no config edit.
func TestTokenFileIsRead(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(path, []byte("  secret\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, config.Auth{TokenFile: path})
	if _, err := c.ListRules(context.Background()); err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
}

func TestAPIErrorCarriesTheServerMessage(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"name is required"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, config.Auth{})
	_, err := c.ListRules(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T, want *APIError", err)
	}
	if apiErr.Status != http.StatusBadRequest || apiErr.Message != "name is required" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
	if Status(err) != http.StatusBadRequest {
		t.Fatalf("Status = %d", Status(err))
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("error text = %q", err)
	}
	if Status(errors.New("plain")) != 0 {
		t.Fatal("Status of a non-API error must be 0")
	}
}

func TestQueryEncodesParameters(t *testing.T) {
	t.Parallel()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, config.Auth{})
	from := time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)
	_, err := c.Query(context.Background(), Query{
		Name:   "cpu",
		Labels: map[string]string{"agent_id": "web-1"},
		From:   from,
		Agg:    "avg",
		Step:   time.Minute,
		Limit:  5,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	for _, want := range []string{"name=cpu", "agent_id=web-1", "from=2026-07-09T10%3A00%3A00Z", "agg=avg", "step=1m0s", "limit=5"} {
		if !strings.Contains(got, want) {
			t.Fatalf("query %q is missing %q", got, want)
		}
	}
}

// A 204 has no body; decoding one would be an error rather than a success.
func TestDeleteAcceptsNoContent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, config.Auth{})
	if err := c.DeleteRule(context.Background(), "x"); err != nil {
		t.Fatalf("DeleteRule: %v", err)
	}
}

// An id with a slash must stay inside its path segment, and must be escaped
// exactly once: assigning an already-escaped path to url.Path would let
// url.String() escape the percent signs again, turning `a/b` into `a%252Fb`.
func TestPathEscapedExactlyOnce(t *testing.T) {
	t.Parallel()
	var rawPath, decodedID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawPath = r.URL.EscapedPath()
		decodedID = strings.TrimPrefix(r.URL.Path, "/api/v1/rules/")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, config.Auth{})
	if err := c.DeleteRule(context.Background(), "a/../b"); err != nil {
		t.Fatalf("DeleteRule: %v", err)
	}
	if strings.Contains(rawPath, "/../") {
		t.Fatalf("escaped path = %q, want the id confined to one segment", rawPath)
	}
	if strings.Contains(rawPath, "%25") {
		t.Fatalf("escaped path = %q, want it escaped exactly once", rawPath)
	}
	if decodedID != "a/../b" {
		t.Fatalf("server decoded the id as %q, want %q", decodedID, "a/../b")
	}
}

// A label may not smuggle in a reserved query parameter.
func TestQueryLabelsCannotOverrideReservedParams(t *testing.T) {
	t.Parallel()
	var q url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query()
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, config.Auth{})
	_, err := c.Query(context.Background(), Query{
		Name:   "cpu",
		Labels: map[string]string{"name": "hijack", "agg": "hijack", "limit": "9999"},
		Agg:    "avg",
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if q.Get("name") != "cpu" {
		t.Fatalf("name = %q, a label hijacked the metric name", q.Get("name"))
	}
	if q.Get("agg") != "avg" {
		t.Fatalf("agg = %q", q.Get("agg"))
	}
	if q.Has("limit") {
		t.Fatalf("a label reintroduced limit = %q", q.Get("limit"))
	}
}

func TestContextCancellation(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, config.Auth{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.ListRules(ctx); err == nil {
		t.Fatal("expected the cancelled context to abort the request")
	}
}

func TestCAFileMustContainCertificates(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := New(&config.Context{Server: "https://example.com", CAFile: path}, 0); err == nil {
		t.Fatal("a ca-file with no certificates was accepted")
	}
	if _, err := New(&config.Context{Server: "https://example.com", CAFile: filepath.Join(t.TempDir(), "absent")}, 0); err == nil {
		t.Fatal("a missing ca-file was accepted")
	}
	// A plain-http context must not even look at the ca-file.
	if _, err := New(&config.Context{Server: "http://example.com", CAFile: path}, 0); err != nil {
		t.Fatalf("http context rejected: %v", err)
	}
}
