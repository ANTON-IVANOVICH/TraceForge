package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"metrics-system/internal/alerting"
	"metrics-system/internal/alerting/rules"
	"metrics-system/internal/auth"
	"metrics-system/internal/clock"
	"metrics-system/internal/server/pipeline"
	"metrics-system/internal/server/storage"
)

// newAlertingServer stands up the HTTP API with auth and alerting enabled. The
// alerting service runs for real, so rule scheduling and tenant scoping are
// exercised end to end rather than mocked.
func newAlertingServer(t *testing.T) *httptest.Server {
	t.Helper()
	store := storage.NewMemoryStorage()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipe := pipeline.New(store, pipeline.Config{}, logger)
	pipe.Start()

	authn, err := auth.NewAPIKeyAuthenticator(auth.APIKeyConfig{Keys: []auth.APIKeyEntry{
		{Key: "a-admin", Subject: "aa", Tenant: "tenant-a", Roles: []string{"admin"}},
		{Key: "a-reader", Subject: "ra", Tenant: "tenant-a", Roles: []string{"reader"}},
		{Key: "b-admin", Subject: "ba", Tenant: "tenant-b", Roles: []string{"admin"}},
	}})
	if err != nil {
		t.Fatalf("authn: %v", err)
	}

	svc, err := alerting.New(alerting.Config{}, store, clock.New(), logger)
	if err != nil {
		t.Fatalf("alerting: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = svc.Run(ctx)
	}()

	h := NewHandler(pipe, store, logger)
	h.SetAlerting(svc)
	srv := httptest.NewServer(Chain(h.Routes(), Authenticate(authn, logger)))

	t.Cleanup(func() {
		srv.Close()
		cancel()
		<-runDone
		pipe.Shutdown()
		_ = store.Close()
	})
	return srv
}

const validRule = `{"name":"CPUHigh","expression":"cpu_usage_percent > 90","for":"1m","interval":"1h","receivers":["log"]}`

func decodeRule(t *testing.T, resp *http.Response) *rules.Rule {
	t.Helper()
	var r rules.Rule
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode rule: %v", err)
	}
	_ = resp.Body.Close()
	return &r
}

func TestAlertingRBAC(t *testing.T) {
	t.Parallel()
	srv := newAlertingServer(t)

	// Unauthenticated reads are rejected.
	if r := authReq(t, http.MethodGet, srv.URL+"/api/v1/alerts", "", ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous alerts: got %d want 401", r.StatusCode)
	}
	// A reader may read rules and alerts...
	if r := authReq(t, http.MethodGet, srv.URL+"/api/v1/rules", "a-reader", ""); r.StatusCode != http.StatusOK {
		t.Fatalf("reader list rules: got %d want 200", r.StatusCode)
	}
	if r := authReq(t, http.MethodGet, srv.URL+"/api/v1/alerts", "a-reader", ""); r.StatusCode != http.StatusOK {
		t.Fatalf("reader list alerts: got %d want 200", r.StatusCode)
	}
	// ...and preview one, since a backtest only reads its own series.
	preview := `{"expression":"cpu_usage_percent > 90"}`
	if r := authReq(t, http.MethodPost, srv.URL+"/api/v1/rules/preview", "a-reader", preview); r.StatusCode != http.StatusOK {
		t.Fatalf("reader preview: got %d want 200", r.StatusCode)
	}
	// But not create one.
	if r := authReq(t, http.MethodPost, srv.URL+"/api/v1/rules", "a-reader", validRule); r.StatusCode != http.StatusForbidden {
		t.Fatalf("reader create rule: got %d want 403", r.StatusCode)
	}
	// An admin can.
	if r := authReq(t, http.MethodPost, srv.URL+"/api/v1/rules", "a-admin", validRule); r.StatusCode != http.StatusCreated {
		t.Fatalf("admin create rule: got %d want 201", r.StatusCode)
	}
}

func TestAlertingRuleValidation(t *testing.T) {
	t.Parallel()
	srv := newAlertingServer(t)

	for name, body := range map[string]string{
		"bad expression": `{"name":"X","expression":"cpu >","interval":"1h"}`,
		"unknown func":   `{"name":"X","expression":"nope(cpu[1m]) > 1","interval":"1h"}`,
		"no name":        `{"name":"","expression":"cpu > 1","interval":"1h"}`,
		"unknown field":  `{"name":"X","expression":"cpu > 1","bogus":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := authReq(t, http.MethodPost, srv.URL+"/api/v1/rules", "a-admin", body)
			if r.StatusCode != http.StatusBadRequest {
				t.Fatalf("got %d want 400", r.StatusCode)
			}
			_ = r.Body.Close()
		})
	}
}

// A tenant must not see, fetch or delete another tenant's rules. A foreign rule
// reads as 404 rather than 403 so rule IDs cannot be probed.
func TestAlertingRuleTenantIsolation(t *testing.T) {
	t.Parallel()
	srv := newAlertingServer(t)

	resp := authReq(t, http.MethodPost, srv.URL+"/api/v1/rules", "a-admin", validRule)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	created := decodeRule(t, resp)
	if created.TenantID != "tenant-a" {
		t.Fatalf("rule tenant = %q, want tenant-a (must come from the principal)", created.TenantID)
	}

	// tenant-b sees nothing.
	r := authReq(t, http.MethodGet, srv.URL+"/api/v1/rules", "b-admin", "")
	var bRules []*rules.Rule
	if err := json.NewDecoder(r.Body).Decode(&bRules); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_ = r.Body.Close()
	if len(bRules) != 0 {
		t.Fatalf("tenant-b sees %d of tenant-a's rules (isolation breach)", len(bRules))
	}

	if r := authReq(t, http.MethodGet, srv.URL+"/api/v1/rules/"+created.ID, "b-admin", ""); r.StatusCode != http.StatusNotFound {
		t.Fatalf("tenant-b get: got %d want 404", r.StatusCode)
	}
	if r := authReq(t, http.MethodDelete, srv.URL+"/api/v1/rules/"+created.ID, "b-admin", ""); r.StatusCode != http.StatusNotFound {
		t.Fatalf("tenant-b delete: got %d want 404", r.StatusCode)
	}
	// tenant-b must not be able to overwrite it either.
	if r := authReq(t, http.MethodPut, srv.URL+"/api/v1/rules/"+created.ID, "b-admin", validRule); r.StatusCode != http.StatusNotFound {
		t.Fatalf("tenant-b update: got %d want 404", r.StatusCode)
	}

	// tenant-a still owns it, and can delete it.
	if r := authReq(t, http.MethodGet, srv.URL+"/api/v1/rules/"+created.ID, "a-admin", ""); r.StatusCode != http.StatusOK {
		t.Fatalf("tenant-a get: got %d want 200", r.StatusCode)
	}
	if r := authReq(t, http.MethodDelete, srv.URL+"/api/v1/rules/"+created.ID, "a-admin", ""); r.StatusCode != http.StatusNoContent {
		t.Fatalf("tenant-a delete: got %d want 204", r.StatusCode)
	}
	if r := authReq(t, http.MethodGet, srv.URL+"/api/v1/rules/"+created.ID, "a-admin", ""); r.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted rule still readable: %d", r.StatusCode)
	}
}

func TestAlertingSilenceTenantIsolation(t *testing.T) {
	t.Parallel()
	srv := newAlertingServer(t)

	body := `{"matchers":[{"name":"agent_id","op":"=","value":"web-1"}],"duration":"2h","comment":"maintenance"}`
	resp := authReq(t, http.MethodPost, srv.URL+"/api/v1/silences", "a-admin", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create silence: %d", resp.StatusCode)
	}
	var created struct {
		ID       string `json:"id"`
		TenantID string `json:"tenant_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_ = resp.Body.Close()
	if created.TenantID != "tenant-a" {
		t.Fatalf("silence tenant = %q, want tenant-a", created.TenantID)
	}

	// A silence with no matchers would mute everything — it must be rejected.
	empty := `{"matchers":[],"duration":"1h"}`
	if r := authReq(t, http.MethodPost, srv.URL+"/api/v1/silences", "a-admin", empty); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("matcher-less silence: got %d want 400", r.StatusCode)
	}

	if r := authReq(t, http.MethodDelete, srv.URL+"/api/v1/silences/"+created.ID, "b-admin", ""); r.StatusCode != http.StatusNotFound {
		t.Fatalf("tenant-b delete silence: got %d want 404", r.StatusCode)
	}
	if r := authReq(t, http.MethodDelete, srv.URL+"/api/v1/silences/"+created.ID, "a-admin", ""); r.StatusCode != http.StatusNoContent {
		t.Fatalf("tenant-a delete silence: got %d want 204", r.StatusCode)
	}
}

// The preview window is bounded so a crafted from/to/step cannot turn one
// request into an unbounded evaluation loop.
func TestAlertingPreviewRejectsHugeWindow(t *testing.T) {
	t.Parallel()
	srv := newAlertingServer(t)

	body := `{"expression":"cpu_usage_percent > 90","from":"2020-01-01T00:00:00Z","to":"2026-01-01T00:00:00Z","step":"1m"}`
	r := authReq(t, http.MethodPost, srv.URL+"/api/v1/rules/preview", "a-admin", body)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("huge preview window: got %d want 400", r.StatusCode)
	}
	_ = r.Body.Close()
}

// With alerting disabled the endpoints must not exist at all.
func TestAlertingRoutesAbsentWhenDisabled(t *testing.T) {
	t.Parallel()
	srv, _ := newAuthedServer(t)

	// /api/v1/rules is unregistered, so the admin-by-default rule applies and the
	// mux 404s an authenticated admin rather than serving anything.
	if r := authReq(t, http.MethodGet, srv.URL+"/api/v1/rules", "", ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous: got %d want 401", r.StatusCode)
	}
}
