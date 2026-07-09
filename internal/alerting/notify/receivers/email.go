package receivers

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"metrics-system/internal/alerting/alert"
)

// defaultEmailTimeout bounds the whole SMTP conversation when none is configured.
const defaultEmailTimeout = 15 * time.Second

// EmailConfig configures the SMTP receiver.
type EmailConfig struct {
	Name     string
	Host     string
	Port     int
	Username string // empty disables authentication
	Password string
	From     string
	To       []string
	Timeout  time.Duration // dial + conversation deadline
}

// Email delivers notifications over SMTP.
//
// net/smtp is old: no context support, PLAIN auth only, and — the part that
// actually bites — smtp.SendMail dials with net.Dial and sets no deadlines, so a
// peer that accepts the connection and then goes silent hangs the caller
// forever. This receiver therefore drives the conversation itself over a
// connection with a dial timeout and an absolute deadline, and closes that
// connection when the context is cancelled. A production deployment would reach
// for a maintained library; here it keeps the dependency list at zero and makes
// the failure mode explicit.
type Email struct {
	name    string
	addr    string
	host    string
	auth    smtp.Auth
	from    string
	to      []string
	timeout time.Duration
	tmpl    *template.Template
}

const emailTemplate = `{{ .Status | printf "%s" }} — {{ .Firing }} firing, {{ .Resolved }} resolved
Group: {{ .GroupLabels }}

{{ range .Alerts }}--------------------------------------------------------------
[{{ .Status }}] {{ .Name }}   (severity: {{ .Severity }})
value:  {{ .Value }}
labels: {{ .LabelsText }}
started: {{ .StartsAt }}
{{ if .Summary }}summary: {{ .Summary }}
{{ end }}{{ end }}
--
TraceForge alerting
`

// NewEmail validates the configuration and compiles the message template.
func NewEmail(cfg EmailConfig) (*Email, error) {
	if cfg.Name == "" {
		return nil, errors.New("email: name is required")
	}
	if cfg.Host == "" {
		return nil, fmt.Errorf("email %s: host is required", cfg.Name)
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("email %s: port %d is out of range", cfg.Name, cfg.Port)
	}
	if cfg.From == "" {
		return nil, fmt.Errorf("email %s: from is required", cfg.Name)
	}
	if len(cfg.To) == 0 {
		return nil, fmt.Errorf("email %s: at least one recipient is required", cfg.Name)
	}
	tmpl, err := template.New("email").Parse(emailTemplate)
	if err != nil {
		return nil, fmt.Errorf("email %s: %w", cfg.Name, err)
	}

	var auth smtp.Auth
	if cfg.Username != "" {
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultEmailTimeout
	}
	return &Email{
		name:    cfg.Name,
		addr:    net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		host:    cfg.Host,
		auth:    auth,
		from:    cfg.From,
		to:      append([]string(nil), cfg.To...),
		timeout: timeout,
		tmpl:    tmpl,
	}, nil
}

// Name identifies the receiver in routing and diagnostics.
func (r *Email) Name() string { return r.name }

// Send renders and delivers the message.
//
// The SMTP conversation runs on its own goroutine because net/smtp is not
// context-aware. That goroutine cannot outlive the call: the connection carries
// an absolute deadline, and a cancelled context closes it, which unblocks any
// read or write in flight. Without both, a peer that accepts and then goes
// silent would strand one goroutine per alert forever.
func (r *Email) Send(ctx context.Context, g *alert.Group) error {
	msg, err := r.render(g)
	if err != nil {
		return Permanent(err)
	}

	dialer := net.Dialer{Timeout: r.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", r.addr)
	if err != nil {
		return err // transient: the server may come back
	}
	if err := conn.SetDeadline(time.Now().Add(r.timeout)); err != nil {
		_ = conn.Close()
		return err
	}

	done := make(chan error, 1) // buffered: the goroutine must never block on send
	go func() { done <- r.converse(conn, msg) }()

	select {
	case <-ctx.Done():
		_ = conn.Close() // unblocks the goroutine's pending read/write
		<-done
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// converse drives one SMTP exchange to completion and closes the connection.
func (r *Email) converse(conn net.Conn, msg []byte) (err error) {
	client, err := smtp.NewClient(conn, r.host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer func() {
		// Quit closes the connection; if it fails, close the socket regardless.
		if qErr := client.Quit(); qErr != nil {
			_ = conn.Close()
		}
	}()

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: r.host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if r.auth != nil {
		if err := client.Auth(r.auth); err != nil {
			// A rejected credential will be rejected again on every retry.
			return Permanent(err)
		}
	}
	if err := client.Mail(r.from); err != nil {
		return err
	}
	for _, addr := range r.to {
		if err := client.Rcpt(addr); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	return w.Close()
}

// alertView is the template's view of one alert.
type alertView struct {
	Status     string
	Name       string
	Severity   string
	Value      float64
	LabelsText string
	StartsAt   time.Time
	Summary    string
}

func (r *Email) render(g *alert.Group) ([]byte, error) {
	firing, resolved := g.Counts()

	views := make([]alertView, 0, len(g.Alerts))
	for _, a := range g.Alerts {
		views = append(views, alertView{
			Status:     string(a.Status),
			Name:       a.Name(),
			Severity:   a.Severity,
			Value:      a.Value,
			LabelsText: alert.LabelsString(a.Labels),
			StartsAt:   a.StartsAt,
			Summary:    a.Annotations["summary"],
		})
	}

	var body strings.Builder
	err := r.tmpl.Execute(&body, struct {
		Status      string
		Firing      int
		Resolved    int
		GroupLabels string
		Alerts      []alertView
	}{
		Status:      strings.ToUpper(string(g.Status())),
		Firing:      firing,
		Resolved:    resolved,
		GroupLabels: alert.LabelsString(g.Labels),
		Alerts:      views,
	})
	if err != nil {
		return nil, fmt.Errorf("render template: %w", err)
	}

	subject := fmt.Sprintf("[%s] %d alert(s): %s",
		strings.ToUpper(string(g.Status())), len(g.Alerts), alert.LabelsString(g.Labels))
	return buildMIME(r.from, r.to, subject, body.String()), nil
}

// buildMIME assembles a minimal RFC 5322 message. Header values are sanitised:
// a newline smuggled into a label would otherwise let an attacker inject extra
// headers (or a second message body) into the mail we send.
func buildMIME(from string, to []string, subject, body string) []byte {
	var b strings.Builder
	b.WriteString("From: " + sanitizeHeader(from) + "\r\n")
	b.WriteString("To: " + sanitizeHeader(strings.Join(to, ", ")) + "\r\n")
	b.WriteString("Subject: " + sanitizeHeader(subject) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return []byte(b.String())
}

func sanitizeHeader(v string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(v)
}
