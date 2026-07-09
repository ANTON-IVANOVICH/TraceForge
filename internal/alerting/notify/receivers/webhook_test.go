package receivers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"metrics-system/internal/alerting/alert"
)

var epoch = time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

func testGroup() *alert.Group {
	end := epoch.Add(time.Minute)
	return &alert.Group{
		Key:      "log|alertname=CPUHigh",
		Receiver: "log",
		Labels:   map[string]string{"alertname": "CPUHigh", "tenant": "tenant-a"},
		Alerts: []*alert.Alert{
			{
				Fingerprint: "f1", RuleName: "CPUHigh", Status: alert.StatusFiring, Severity: "critical",
				Labels:      map[string]string{"alertname": "CPUHigh", "agent_id": "web-1"},
				Annotations: map[string]string{"summary": "CPU is 95%"},
				Value:       95, StartsAt: epoch,
			},
			{
				Fingerprint: "f2", RuleName: "CPUHigh", Status: alert.StatusResolved, Severity: "warning",
				Labels: map[string]string{"alertname": "CPUHigh", "agent_id": "web-2"},
				Value:  10, StartsAt: epoch, EndsAt: &end,
			},
		},
		UpdatedAt: epoch,
	}
}

func TestWebhookPayloadShape(t *testing.T) {
	t.Parallel()
	var body []byte
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		contentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh, err := NewWebhook(WebhookConfig{Name: "ops", URL: srv.URL}, srv.Client())
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	if err := wh.Send(context.Background(), testGroup()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if contentType != "application/json" {
		t.Fatalf("Content-Type = %q", contentType)
	}
	var got webhookPayload
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Version != "1" || got.Receiver != "log" || got.GroupKey != "log|alertname=CPUHigh" {
		t.Fatalf("payload = %+v", got)
	}
	// The group has one firing alert, so its status is firing.
	if got.Status != "firing" {
		t.Fatalf("status = %q, want firing", got.Status)
	}
	if len(got.Alerts) != 2 {
		t.Fatalf("got %d alerts, want 2", len(got.Alerts))
	}
	if got.GroupLabels["tenant"] != "tenant-a" {
		t.Fatalf("group labels = %v", got.GroupLabels)
	}
}

// Without a signature anyone who learns the URL can forge alerts, so verify the
// HMAC exactly as a subscriber would.
func TestWebhookHMACSignature(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t"

	verified := make(chan bool, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts := r.Header.Get("X-TraceForge-Timestamp")
		sig := r.Header.Get("X-TraceForge-Signature")

		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts))
		mac.Write([]byte("."))
		mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))

		// The timestamp must be signed too, or a captured request replays forever.
		if _, err := strconv.ParseInt(ts, 10, 64); err != nil {
			verified <- false
			return
		}
		verified <- hmac.Equal([]byte(sig), []byte(want))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh, err := NewWebhook(WebhookConfig{Name: "ops", URL: srv.URL, Secret: secret}, srv.Client())
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	if err := wh.Send(context.Background(), testGroup()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !<-verified {
		t.Fatal("signature did not verify")
	}
}

func TestWebhookOmitsSignatureWithoutSecret(t *testing.T) {
	t.Parallel()
	var sig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sig = r.Header.Get("X-TraceForge-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh, _ := NewWebhook(WebhookConfig{Name: "ops", URL: srv.URL}, srv.Client())
	if err := wh.Send(context.Background(), testGroup()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sig != "" {
		t.Fatalf("unsigned webhook carried a signature header %q", sig)
	}
}

func TestWebhookCustomHeaders(t *testing.T) {
	t.Parallel()
	var env string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env = r.Header.Get("X-Environment")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	wh, _ := NewWebhook(WebhookConfig{Name: "ops", URL: srv.URL, Headers: map[string]string{"X-Environment": "staging"}}, srv.Client())
	if err := wh.Send(context.Background(), testGroup()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if env != "staging" {
		t.Fatalf("X-Environment = %q", env)
	}
}

// Retrying a 400 or a 401 cannot help; 408, 429 and 5xx explicitly invite one.
func TestWebhookClassifiesStatusCodes(t *testing.T) {
	t.Parallel()
	tests := map[int]struct {
		wantErr       bool
		wantPermanent bool
	}{
		200: {false, false},
		204: {false, false},
		400: {true, true},
		401: {true, true},
		404: {true, true},
		408: {true, false},
		429: {true, false},
		500: {true, false},
		503: {true, false},
	}
	for code, want := range tests {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
				_, _ = w.Write([]byte("detail"))
			}))
			defer srv.Close()

			wh, _ := NewWebhook(WebhookConfig{Name: "ops", URL: srv.URL}, srv.Client())
			err := wh.Send(context.Background(), testGroup())

			if want.wantErr != (err != nil) {
				t.Fatalf("status %d: err = %v, wantErr = %v", code, err, want.wantErr)
			}
			if err != nil && IsPermanent(err) != want.wantPermanent {
				t.Fatalf("status %d: permanent = %v, want %v", code, IsPermanent(err), want.wantPermanent)
			}
		})
	}
}

func TestWebhookHonoursContextCancellation(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh, _ := NewWebhook(WebhookConfig{Name: "ops", URL: srv.URL}, srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := wh.Send(ctx, testGroup()); err == nil {
		t.Fatal("expected the cancelled context to abort the send")
	}
}

func TestWebhookConfigValidation(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]WebhookConfig{
		"no name":    {URL: "http://example.com"},
		"no url":     {Name: "ops"},
		"bad scheme": {Name: "ops", URL: "ftp://example.com"},
		"no host":    {Name: "ops", URL: "http://"},
		"unparsable": {Name: "ops", URL: "://nope"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewWebhook(cfg, nil); err == nil {
				t.Fatal("invalid config was accepted")
			}
		})
	}
	if _, err := NewWebhook(WebhookConfig{Name: "ops", URL: "https://example.com/hook"}, nil); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}
