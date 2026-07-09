// Package config loads the alerting configuration — receivers, routing defaults
// and inhibition rules — plus the bootstrap rule file, and turns them into the
// live objects the alerting service runs on. JSON parsing is strict: an unknown
// field is a typo, and a silently ignored typo in a notification config is
// discovered at the worst possible moment.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"metrics-system/internal/alerting/inhibit"
	"metrics-system/internal/alerting/notify/receivers"
	"metrics-system/internal/alerting/rules"
)

// File is the on-disk alerting configuration (-alert-config).
type File struct {
	GroupBy          []string         `json:"group_by,omitempty"`
	GroupWait        rules.Duration   `json:"group_wait,omitempty"`
	GroupInterval    rules.Duration   `json:"group_interval,omitempty"`
	RepeatInterval   rules.Duration   `json:"repeat_interval,omitempty"`
	DefaultReceivers []string         `json:"default_receivers,omitempty"`
	Receivers        []ReceiverConfig `json:"receivers"`
	InhibitRules     []inhibit.Rule   `json:"inhibit_rules,omitempty"`
}

// ReceiverConfig describes one notification destination. Which fields apply
// depends on Type; the rest must be absent.
type ReceiverConfig struct {
	Name string `json:"name"`
	Type string `json:"type"` // log | webhook | slack | email

	// webhook
	URL     string            `json:"url,omitempty"`
	Secret  string            `json:"secret,omitempty"` // HMAC signing key
	Headers map[string]string `json:"headers,omitempty"`

	// slack
	WebhookURL string `json:"webhook_url,omitempty"`
	Channel    string `json:"channel,omitempty"`
	Username   string `json:"username,omitempty"`

	// email
	Host         string   `json:"host,omitempty"`
	Port         int      `json:"port,omitempty"`
	SMTPUsername string   `json:"smtp_username,omitempty"`
	Password     string   `json:"password,omitempty"`
	From         string   `json:"from,omitempty"`
	To           []string `json:"to,omitempty"`

	Timeout rules.Duration `json:"timeout,omitempty"`
}

// RulesFile is the on-disk bootstrap rule set (-alert-rules).
type RulesFile struct {
	Rules []*rules.Rule `json:"rules"`
}

// Load reads and validates the alerting configuration.
func Load(path string) (*File, error) {
	var f File
	if err := readStrictJSON(path, &f); err != nil {
		return nil, err
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Default returns the zero-configuration setup: a single receiver that logs
// every notification, so alerting is demonstrable without an SMTP or Slack.
func Default() *File {
	return &File{
		DefaultReceivers: []string{"log"},
		Receivers:        []ReceiverConfig{{Name: "log", Type: "log"}},
	}
}

// Validate checks receiver names are unique and non-empty, that every default
// receiver exists, and that each inhibition rule is well formed.
func (f *File) Validate() error {
	if len(f.Receivers) == 0 {
		return errors.New("at least one receiver is required")
	}
	seen := make(map[string]bool, len(f.Receivers))
	for i, rc := range f.Receivers {
		if rc.Name == "" {
			return fmt.Errorf("receivers[%d]: name is required", i)
		}
		if seen[rc.Name] {
			return fmt.Errorf("receivers[%d]: duplicate receiver name %q", i, rc.Name)
		}
		seen[rc.Name] = true
	}
	for _, name := range f.DefaultReceivers {
		if !seen[name] {
			return fmt.Errorf("default_receivers: unknown receiver %q", name)
		}
	}
	for i, ir := range f.InhibitRules {
		if err := ir.Validate(); err != nil {
			return fmt.Errorf("inhibit_rules[%d]: %w", i, err)
		}
	}
	if len(f.DefaultReceivers) == 0 {
		return errors.New("default_receivers must name at least one receiver")
	}
	return nil
}

// BuildReceivers instantiates every configured receiver.
func BuildReceivers(f *File, logger *slog.Logger) ([]receivers.Receiver, error) {
	out := make([]receivers.Receiver, 0, len(f.Receivers))
	for i, rc := range f.Receivers {
		r, err := buildReceiver(rc, logger)
		if err != nil {
			return nil, fmt.Errorf("receivers[%d] (%s): %w", i, rc.Name, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func buildReceiver(rc ReceiverConfig, logger *slog.Logger) (receivers.Receiver, error) {
	timeout := rc.Timeout.D()
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	client := &http.Client{Timeout: timeout}

	switch rc.Type {
	case "log":
		return receivers.NewLog(rc.Name, logger), nil
	case "webhook":
		return receivers.NewWebhook(receivers.WebhookConfig{
			Name:    rc.Name,
			URL:     rc.URL,
			Secret:  rc.Secret,
			Headers: rc.Headers,
			Timeout: timeout,
		}, client)
	case "slack":
		return receivers.NewSlack(receivers.SlackConfig{
			Name:       rc.Name,
			WebhookURL: rc.WebhookURL,
			Channel:    rc.Channel,
			Username:   rc.Username,
			Timeout:    timeout,
		}, client)
	case "email":
		return receivers.NewEmail(receivers.EmailConfig{
			Name:     rc.Name,
			Host:     rc.Host,
			Port:     rc.Port,
			Username: rc.SMTPUsername,
			Password: rc.Password,
			From:     rc.From,
			To:       rc.To,
			Timeout:  rc.Timeout.D(),
		})
	default:
		return nil, fmt.Errorf("unknown receiver type %q (want log|webhook|slack|email)", rc.Type)
	}
}

// LoadRules reads the bootstrap rule file and compiles every rule, so a typo in
// an expression fails at startup rather than silently never firing.
func LoadRules(path string) ([]*rules.Rule, error) {
	var f RulesFile
	if err := readStrictJSON(path, &f); err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(f.Rules))
	for i, r := range f.Rules {
		if r == nil {
			return nil, fmt.Errorf("rules[%d]: null", i)
		}
		if r.ID == "" {
			return nil, fmt.Errorf("rules[%d]: id is required", i)
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("rules[%d]: duplicate rule id %q", i, r.ID)
		}
		seen[r.ID] = true
		if err := r.Compile(); err != nil {
			return nil, fmt.Errorf("rules[%d] (%s): %w", i, r.ID, err)
		}
	}
	return f.Rules, nil
}

// readStrictJSON decodes path into v, rejecting unknown fields and trailing data.
func readStrictJSON(path string, v any) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	dec := json.NewDecoder(file)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("parse %s: unexpected trailing data", path)
	}
	return nil
}
