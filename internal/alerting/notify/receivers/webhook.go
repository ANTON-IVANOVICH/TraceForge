package receivers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"metrics-system/internal/alerting/alert"
)

// maxErrorBody bounds how much of a failing response we read into the error.
const maxErrorBody = 1 << 10

// WebhookConfig configures the generic receiver.
type WebhookConfig struct {
	Name    string
	URL     string
	Secret  string // when set, payloads are HMAC-signed
	Headers map[string]string
	Timeout time.Duration
}

// Webhook POSTs the alert group as JSON to an arbitrary URL. It is the universal
// integration point: PagerDuty, Opsgenie, Discord and in-house systems all speak
// it.
//
// It deliberately uses a plain *http.Client rather than pkg/httpx: httpx retries
// internally, and retrying is this package's job (with backoff, jitter and a
// circuit breaker). Two independent retry layers would multiply attempts.
type Webhook struct {
	name    string
	url     string
	secret  []byte
	headers map[string]string
	client  *http.Client
}

// webhookPayload is the wire contract for subscribers. It carries a version so
// the shape can evolve without silently breaking consumers.
type webhookPayload struct {
	Version     string            `json:"version"`
	GroupKey    string            `json:"group_key"`
	Receiver    string            `json:"receiver"`
	Status      string            `json:"status"`
	GroupLabels map[string]string `json:"group_labels"`
	Alerts      []*alert.Alert    `json:"alerts"`
}

// NewWebhook validates the configuration and returns a receiver.
func NewWebhook(cfg WebhookConfig, client *http.Client) (*Webhook, error) {
	if cfg.Name == "" {
		return nil, errors.New("webhook: name is required")
	}
	if err := validateHTTPURL(cfg.URL); err != nil {
		return nil, fmt.Errorf("webhook %s: %w", cfg.Name, err)
	}
	if client == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}
	return &Webhook{
		name:    cfg.Name,
		url:     cfg.URL,
		secret:  []byte(cfg.Secret),
		headers: cfg.Headers,
		client:  client,
	}, nil
}

// Name identifies the receiver in routing and diagnostics.
func (r *Webhook) Name() string { return r.name }

// Send posts the group, signing it when a secret is configured.
func (r *Webhook) Send(ctx context.Context, g *alert.Group) error {
	body, err := json.Marshal(webhookPayload{
		Version:     "1",
		GroupKey:    g.Key,
		Receiver:    g.Receiver,
		Status:      string(g.Status()),
		GroupLabels: g.Labels,
		Alerts:      g.Alerts,
	})
	if err != nil {
		return Permanent(fmt.Errorf("marshal payload: %w", err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return Permanent(err)
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")

	if len(r.secret) > 0 {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		req.Header.Set("X-TraceForge-Timestamp", ts)
		req.Header.Set("X-TraceForge-Signature", "sha256="+sign(r.secret, ts, body))
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return err // network failures are transient
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return classify(r.name, resp)
}

// sign computes the HMAC over "<timestamp>.<body>". Including the timestamp in
// the signed material is what stops a captured request from being replayed later
// — the same construction GitHub and Stripe use for their webhooks. Without a
// signature, anyone who learns the URL can forge alerts.
func sign(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// classify turns a response into nil, a transient error or a permanent one.
// Retrying a 400 or a 401 cannot help: the payload or the credential is wrong,
// and every attempt just burns the retry budget against a service that already
// said no. 408 and 429 are the exceptions — those explicitly invite a retry.
func classify(name string, resp *http.Response) error {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode >= 500:
		return fmt.Errorf("%s returned %d: %s", name, resp.StatusCode, errorBody(resp))
	default:
		return Permanentf("%s returned %d: %s", name, resp.StatusCode, errorBody(resp))
	}
}

func errorBody(resp *http.Response) string {
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return ""
	}
	return string(bytes.TrimSpace(b))
}

func validateHTTPURL(raw string) error {
	if raw == "" {
		return errors.New("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("url has no host")
	}
	return nil
}
