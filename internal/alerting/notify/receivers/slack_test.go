package receivers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"metrics-system/internal/alerting/alert"
)

func TestSlackPayload(t *testing.T) {
	t.Parallel()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sl, err := NewSlack(SlackConfig{Name: "slack", WebhookURL: srv.URL, Channel: "#alerts", Username: "TraceForge"}, srv.Client())
	if err != nil {
		t.Fatalf("NewSlack: %v", err)
	}
	if err := sl.Send(context.Background(), testGroup()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var got slackPayload
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Channel != "#alerts" || got.Username != "TraceForge" {
		t.Fatalf("payload = %+v", got)
	}
	if !strings.Contains(got.Text, "1 firing") || !strings.Contains(got.Text, "1 resolved") {
		t.Fatalf("text = %q, want the firing/resolved counts", got.Text)
	}
	if len(got.Attachments) != 2 {
		t.Fatalf("got %d attachments, want 2", len(got.Attachments))
	}
	// Colour encodes urgency: critical is danger, a resolved alert is good.
	if got.Attachments[0].Color != "danger" {
		t.Fatalf("firing critical colour = %q, want danger", got.Attachments[0].Color)
	}
	if got.Attachments[1].Color != "good" {
		t.Fatalf("resolved colour = %q, want good", got.Attachments[1].Color)
	}
	if got.Attachments[0].Text != "CPU is 95%" {
		t.Fatalf("attachment text = %q, want the summary annotation", got.Attachments[0].Text)
	}
}

// A rack of failed hosts must not become fifty Slack cards.
func TestSlackCapsAttachments(t *testing.T) {
	t.Parallel()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g := testGroup()
	g.Alerts = nil
	for i := 0; i < 25; i++ {
		g.Alerts = append(g.Alerts, &alert.Alert{
			Fingerprint: string(rune('a' + i)), RuleName: "CPUHigh", Status: alert.StatusFiring,
			Labels: map[string]string{"alertname": "CPUHigh"}, StartsAt: epoch,
		})
	}

	sl, _ := NewSlack(SlackConfig{Name: "slack", WebhookURL: srv.URL}, srv.Client())
	if err := sl.Send(context.Background(), g); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var got slackPayload
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Attachments) != maxAttachments {
		t.Fatalf("got %d attachments, want the cap of %d", len(got.Attachments), maxAttachments)
	}
	if !strings.Contains(got.Text, "and 15 more") {
		t.Fatalf("text = %q, want it to mention the truncated alerts", got.Text)
	}
}

func TestSlackClassifiesErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	sl, _ := NewSlack(SlackConfig{Name: "slack", WebhookURL: srv.URL}, srv.Client())
	err := sl.Send(context.Background(), testGroup())
	if err == nil {
		t.Fatal("expected a 429 to be an error")
	}
	if IsPermanent(err) {
		t.Fatal("a 429 must be transient — it explicitly invites a retry")
	}
}

func TestSlackConfigValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewSlack(SlackConfig{WebhookURL: "https://hooks.slack.com/x"}, nil); err == nil {
		t.Fatal("a nameless receiver was accepted")
	}
	if _, err := NewSlack(SlackConfig{Name: "slack", WebhookURL: "not a url"}, nil); err == nil {
		t.Fatal("an invalid webhook url was accepted")
	}
}

func TestLogReceiver(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	logger := slog.New(slog.NewTextHandler(&sb, nil))

	r := NewLog("", logger)
	if r.Name() != "log" {
		t.Fatalf("Name = %q, want the default", r.Name())
	}
	if err := r.Send(context.Background(), testGroup()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "CPUHigh") || !strings.Contains(out, "firing=1") {
		t.Fatalf("log output = %q", out)
	}
}

// The summary is capped so a fifty-host incident does not produce a fifty-line
// log record.
func TestLogReceiverCapsSummary(t *testing.T) {
	t.Parallel()
	g := testGroup()
	g.Alerts = nil
	for i := 0; i < 20; i++ {
		g.Alerts = append(g.Alerts, &alert.Alert{
			Fingerprint: string(rune('a' + i)), RuleName: "X", Status: alert.StatusFiring,
			Labels: map[string]string{"alertname": "X"}, StartsAt: epoch,
		})
	}
	lines := summarize(g)
	if len(lines) != 11 || lines[10] != "…" {
		t.Fatalf("summarize produced %d lines, want 10 plus an ellipsis", len(lines))
	}
}

func TestEmailConfigValidation(t *testing.T) {
	t.Parallel()
	valid := EmailConfig{Name: "mail", Host: "smtp.example.com", Port: 587, From: "a@b.c", To: []string{"d@e.f"}}
	if _, err := NewEmail(valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	for name, mutate := range map[string]func(*EmailConfig){
		"no name":      func(c *EmailConfig) { c.Name = "" },
		"no host":      func(c *EmailConfig) { c.Host = "" },
		"bad port":     func(c *EmailConfig) { c.Port = 0 },
		"huge port":    func(c *EmailConfig) { c.Port = 70000 },
		"no from":      func(c *EmailConfig) { c.From = "" },
		"no recipient": func(c *EmailConfig) { c.To = nil },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			cfg.To = append([]string(nil), valid.To...)
			mutate(&cfg)
			if _, err := NewEmail(cfg); err == nil {
				t.Fatal("invalid config was accepted")
			}
		})
	}
}

// A newline smuggled into a header would let an attacker inject extra headers.
func TestEmailHeaderInjectionIsSanitised(t *testing.T) {
	t.Parallel()
	msg := buildMIME("a@b.c", []string{"d@e.f"}, "subject\r\nBcc: attacker@evil.com", "body")
	headers, _, _ := strings.Cut(string(msg), "\r\n\r\n")

	// The injected text must survive only as part of the Subject value, never as
	// a header line of its own.
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Fatalf("header injection succeeded:\n%s", headers)
		}
	}
	if !strings.Contains(headers, "Subject: subject  Bcc: attacker@evil.com") {
		t.Fatalf("newlines were not folded into the subject:\n%s", headers)
	}
}

func TestEmailRenderIncludesAlerts(t *testing.T) {
	t.Parallel()
	e, err := NewEmail(EmailConfig{Name: "mail", Host: "h", Port: 25, From: "a@b.c", To: []string{"d@e.f"}})
	if err != nil {
		t.Fatalf("NewEmail: %v", err)
	}
	msg, err := e.render(testGroup())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	body := string(msg)
	for _, want := range []string{"FIRING", "CPUHigh", "web-1", "CPU is 95%", "Subject: [FIRING] 2 alert(s)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("message is missing %q:\n%s", want, body)
		}
	}
}

// Send must give up when the context expires, even though smtp.SendMail cannot.
func TestEmailHonoursContextCancellation(t *testing.T) {
	t.Parallel()
	// Port 1 refuses or hangs; either way the context deadline must win.
	e, err := NewEmail(EmailConfig{Name: "mail", Host: "192.0.2.1", Port: 25, From: "a@b.c", To: []string{"d@e.f"}})
	if err != nil {
		t.Fatalf("NewEmail: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := e.Send(ctx, testGroup()); err == nil {
		t.Fatal("expected an error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Send blocked for %v — it must return when the context expires", elapsed)
	}
}
