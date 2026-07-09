// Package client talks to a TraceForge server's HTTP API on behalf of the CLI.
// It is deliberately thin: one method per endpoint, returning the server's own
// types, so a command reads as "call this, print that".
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"metrics-system/internal/alerting/rules"
	"metrics-system/internal/alerting/silence"
	"metrics-system/internal/cli/config"
	"metrics-system/internal/model"
)

// maxErrorBody bounds how much of a failing response we read into the error.
const maxErrorBody = 4 << 10

// APIError is a non-2xx response. Callers map its Status onto an exit code.
type APIError struct {
	Status  int
	Message string
	Method  string
	Path    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s %s: %s", e.Method, e.Path, http.StatusText(e.Status))
	}
	return fmt.Sprintf("%s %s: %s", e.Method, e.Path, e.Message)
}

// Client is an HTTP client bound to one server and credential.
type Client struct {
	base    *url.URL
	http    *http.Client
	apiKey  string
	token   string
	timeout time.Duration
}

// New builds a client for a config context.
func New(ctx *config.Context, timeout time.Duration) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(ctx.Server, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid server url %q: %w", ctx.Server, err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("server url must be http or https, got %q", ctx.Server)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("server url %q has no host", ctx.Server)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	transport := &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
	}
	if base.Scheme == "https" {
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if ctx.Insecure {
			tlsCfg.InsecureSkipVerify = true
		}
		if ctx.CAFile != "" {
			pem, err := os.ReadFile(ctx.CAFile)
			if err != nil {
				return nil, fmt.Errorf("read ca-file: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("ca-file %s contains no certificates", ctx.CAFile)
			}
			tlsCfg.RootCAs = pool
		}
		transport.TLSClientConfig = tlsCfg
	}

	apiKey, token, err := ctx.Credential()
	if err != nil {
		return nil, err
	}
	return &Client{
		base:    base,
		http:    &http.Client{Timeout: timeout, Transport: transport},
		apiKey:  apiKey,
		token:   token,
		timeout: timeout,
	}, nil
}

// Server reports the endpoint this client talks to (for diagnostics).
func (c *Client) Server() string { return c.base.String() }

// do performs a request and decodes a JSON body into out (may be nil).
//
// path arrives already percent-escaped (ids go through url.PathEscape). Setting
// only u.Path would escape it a second time — `a/b` becoming `a%252Fb` — so the
// escaped form goes to RawPath and the decoded form to Path, which is exactly
// the pair url.URL.EscapedPath expects.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := *c.base
	escaped := strings.TrimRight(u.Path, "/") + path
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	u.Path = decoded
	u.RawPath = escaped
	if query != nil {
		u.RawQuery = query.Encode()
	}

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Message: errorMessage(resp), Method: method, Path: path}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

// errorMessage extracts the server's `{"error": "..."}` payload, falling back to
// the raw body.
func errorMessage(resp *http.Response) string {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil || len(data) == 0 {
		return ""
	}
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &payload) == nil && payload.Error != "" {
		return payload.Error
	}
	return strings.TrimSpace(string(data))
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

// Query describes a read against the metric store.
type Query struct {
	Name   string
	Labels map[string]string
	From   time.Time
	To     time.Time
	Agg    string
	Step   time.Duration
	Limit  int
}

// Query returns the metrics matching q.
func (c *Client) Query(ctx context.Context, q Query) ([]model.Metric, error) {
	// Labels go in first and every reserved parameter is then dropped, so a label
	// named `name` or `agg` can never hijack the query. The CLI rejects such a
	// label up front; this is the belt to that pair of braces.
	v := url.Values{}
	for k, val := range q.Labels {
		v.Set(k, val)
	}
	for _, reserved := range []string{"name", "from", "to", "agg", "step", "limit"} {
		v.Del(reserved)
	}

	v.Set("name", q.Name)
	if !q.From.IsZero() {
		v.Set("from", q.From.UTC().Format(time.RFC3339))
	}
	if !q.To.IsZero() {
		v.Set("to", q.To.UTC().Format(time.RFC3339))
	}
	if q.Agg != "" {
		v.Set("agg", q.Agg)
	}
	if q.Step > 0 {
		v.Set("step", q.Step.String())
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}

	var out []model.Metric
	if err := c.do(ctx, http.MethodGet, "/api/v1/query", v, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Stats is the server's self-report.
type Stats struct {
	Pipeline struct {
		Ingested int64 `json:"ingested"`
		Dropped  int64 `json:"dropped"`
		Invalid  int64 `json:"invalid"`
		Stored   int64 `json:"stored"`
	} `json:"pipeline"`
	Storage struct {
		Series int   `json:"series"`
		Points int64 `json:"points"`
	} `json:"storage"`
}

// Stats fetches /debug/stats.
func (c *Client) Stats(ctx context.Context) (*Stats, error) {
	var out Stats
	if err := c.do(ctx, http.MethodGet, "/debug/stats", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Rules
// ---------------------------------------------------------------------------

// RuleSpec is the wire shape for creating or updating a rule. It mirrors the
// server's request body; Enabled is a pointer so an omitted field means "yes".
type RuleSpec struct {
	ID          string            `json:"id,omitempty"`
	Name        string            `json:"name"`
	Expression  string            `json:"expression"`
	For         *rules.Duration   `json:"for,omitempty"`
	Interval    *rules.Duration   `json:"interval,omitempty"`
	Severity    string            `json:"severity,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Receivers   []string          `json:"receivers,omitempty"`
	Enabled     *bool             `json:"enabled,omitempty"`
}

// ListRules returns the caller's rules.
func (c *Client) ListRules(ctx context.Context) ([]*rules.Rule, error) {
	var out []*rules.Rule
	if err := c.do(ctx, http.MethodGet, "/api/v1/rules", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetRule returns one rule.
func (c *Client) GetRule(ctx context.Context, id string) (*rules.Rule, error) {
	var out rules.Rule
	if err := c.do(ctx, http.MethodGet, "/api/v1/rules/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateRule posts a new rule.
func (c *Client) CreateRule(ctx context.Context, spec RuleSpec) (*rules.Rule, error) {
	var out rules.Rule
	if err := c.do(ctx, http.MethodPost, "/api/v1/rules", nil, spec, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateRule replaces a rule by ID.
func (c *Client) UpdateRule(ctx context.Context, id string, spec RuleSpec) (*rules.Rule, error) {
	var out rules.Rule
	if err := c.do(ctx, http.MethodPut, "/api/v1/rules/"+url.PathEscape(id), nil, spec, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteRule removes a rule.
func (c *Client) DeleteRule(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/rules/"+url.PathEscape(id), nil, nil, nil)
}

// PreviewRequest backtests an expression.
type PreviewRequest struct {
	Expression string          `json:"expression"`
	From       *time.Time      `json:"from,omitempty"`
	To         *time.Time      `json:"to,omitempty"`
	Step       *rules.Duration `json:"step,omitempty"`
}

// PreviewResponse is what a backtest produced.
type PreviewResponse struct {
	Count   int `json:"count"`
	Results []struct {
		At      time.Time      `json:"at"`
		Samples []rules.Sample `json:"samples"`
	} `json:"results"`
}

// PreviewRule evaluates an expression without saving it.
func (c *Client) PreviewRule(ctx context.Context, req PreviewRequest) (*PreviewResponse, error) {
	var out PreviewResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/rules/preview", nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Alerts and silences
// ---------------------------------------------------------------------------

// ListAlerts returns the pending and firing alerts.
func (c *Client) ListAlerts(ctx context.Context) ([]*rules.AlertState, error) {
	var out []*rules.AlertState
	if err := c.do(ctx, http.MethodGet, "/api/v1/alerts", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SilenceSpec creates a silence. Duration is a convenience for EndsAt.
type SilenceSpec struct {
	Matchers  []silence.Matcher `json:"matchers"`
	StartsAt  *time.Time        `json:"starts_at,omitempty"`
	EndsAt    *time.Time        `json:"ends_at,omitempty"`
	Duration  *rules.Duration   `json:"duration,omitempty"`
	CreatedBy string            `json:"created_by,omitempty"`
	Comment   string            `json:"comment,omitempty"`
}

// ListSilences returns the caller's silences.
func (c *Client) ListSilences(ctx context.Context) ([]*silence.Silence, error) {
	var out []*silence.Silence
	if err := c.do(ctx, http.MethodGet, "/api/v1/silences", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateSilence posts a new silence.
func (c *Client) CreateSilence(ctx context.Context, spec SilenceSpec) (*silence.Silence, error) {
	var out silence.Silence
	if err := c.do(ctx, http.MethodPost, "/api/v1/silences", nil, spec, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSilence removes a silence.
func (c *Client) DeleteSilence(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/silences/"+url.PathEscape(id), nil, nil, nil)
}

// Status extracts the HTTP status from an error, or 0.
func Status(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}
