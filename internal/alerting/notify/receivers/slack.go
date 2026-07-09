package receivers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"metrics-system/internal/alerting/alert"
)

// maxAttachments caps how many alerts are rendered individually; the rest are
// summarised. A rack of fifty failed hosts should not produce fifty Slack cards.
const maxAttachments = 10

// SlackConfig configures the Slack incoming-webhook receiver.
type SlackConfig struct {
	Name       string
	WebhookURL string
	Channel    string // optional override
	Username   string // optional display name
	Timeout    time.Duration
}

// Slack posts an incoming-webhook message with one attachment per alert.
type Slack struct {
	name     string
	url      string
	channel  string
	username string
	client   *http.Client
}

type slackPayload struct {
	Channel     string            `json:"channel,omitempty"`
	Username    string            `json:"username,omitempty"`
	Text        string            `json:"text"`
	Attachments []slackAttachment `json:"attachments,omitempty"`
}

type slackAttachment struct {
	Color  string       `json:"color"`
	Title  string       `json:"title"`
	Text   string       `json:"text,omitempty"`
	Fields []slackField `json:"fields,omitempty"`
	TS     int64        `json:"ts,omitempty"`
}

type slackField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

// NewSlack validates the configuration and returns a receiver.
func NewSlack(cfg SlackConfig, client *http.Client) (*Slack, error) {
	if cfg.Name == "" {
		return nil, errors.New("slack: name is required")
	}
	if err := validateHTTPURL(cfg.WebhookURL); err != nil {
		return nil, fmt.Errorf("slack %s: %w", cfg.Name, err)
	}
	if client == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}
	return &Slack{
		name:     cfg.Name,
		url:      cfg.WebhookURL,
		channel:  cfg.Channel,
		username: cfg.Username,
		client:   client,
	}, nil
}

// Name identifies the receiver in routing and diagnostics.
func (r *Slack) Name() string { return r.name }

// Send posts the group to the Slack webhook.
func (r *Slack) Send(ctx context.Context, g *alert.Group) error {
	body, err := json.Marshal(r.payload(g))
	if err != nil {
		return Permanent(fmt.Errorf("marshal payload: %w", err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return Permanent(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return classify(r.name, resp)
}

func (r *Slack) payload(g *alert.Group) slackPayload {
	firing, resolved := g.Counts()
	text := fmt.Sprintf("*%s* — %d firing, %d resolved · %s",
		string(g.Status()), firing, resolved, alert.LabelsString(g.Labels))

	attachments := make([]slackAttachment, 0, min(len(g.Alerts), maxAttachments))
	for i, a := range g.Alerts {
		if i == maxAttachments {
			text += fmt.Sprintf("\n… and %d more", len(g.Alerts)-maxAttachments)
			break
		}
		attachments = append(attachments, slackAttachment{
			Color:  slackColor(a),
			Title:  fmt.Sprintf("[%s] %s", a.Status, a.Name()),
			Text:   a.Annotations["summary"],
			Fields: slackFields(a),
			TS:     a.StartsAt.Unix(),
		})
	}
	return slackPayload{
		Channel:     r.channel,
		Username:    r.username,
		Text:        text,
		Attachments: attachments,
	}
}

func slackColor(a *alert.Alert) string {
	if a.Status == alert.StatusResolved {
		return "good"
	}
	if a.Severity == "critical" {
		return "danger"
	}
	return "warning"
}

func slackFields(a *alert.Alert) []slackField {
	keys := make([]string, 0, len(a.Labels))
	for k := range a.Labels {
		if k != "alertname" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	fields := make([]slackField, 0, len(keys)+1)
	fields = append(fields, slackField{Title: "value", Value: fmt.Sprintf("%g", a.Value), Short: true})
	for _, k := range keys {
		fields = append(fields, slackField{Title: k, Value: a.Labels[k], Short: true})
	}
	return fields
}
