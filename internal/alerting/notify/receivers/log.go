package receivers

import (
	"context"
	"log/slog"

	"metrics-system/internal/alerting/alert"
)

// Log writes notifications to the server log. It is the default receiver, which
// makes the whole alerting stack demonstrable without an SMTP server, a Slack
// workspace or a webhook endpoint.
type Log struct {
	name   string
	logger *slog.Logger
}

// NewLog returns a logging receiver.
func NewLog(name string, logger *slog.Logger) *Log {
	if logger == nil {
		logger = slog.Default()
	}
	if name == "" {
		name = "log"
	}
	return &Log{name: name, logger: logger}
}

// Name identifies the receiver in routing and diagnostics.
func (r *Log) Name() string { return r.name }

// Send logs the group. It never fails, so it never exercises retry or the
// circuit breaker.
func (r *Log) Send(_ context.Context, g *alert.Group) error {
	firing, resolved := g.Counts()
	r.logger.Info("alert notification",
		"receiver", r.name,
		"group", g.Key,
		"status", string(g.Status()),
		"firing", firing,
		"resolved", resolved,
		"labels", alert.LabelsString(g.Labels),
		"alerts", summarize(g))
	return nil
}

// summarize renders one line per alert, capped so a fifty-host incident does not
// produce a fifty-line log record.
func summarize(g *alert.Group) []string {
	const maxListed = 10
	out := make([]string, 0, min(len(g.Alerts), maxListed))
	for i, a := range g.Alerts {
		if i == maxListed {
			out = append(out, "…")
			break
		}
		out = append(out, string(a.Status)+" "+a.Name()+" "+alert.LabelsString(a.Labels))
	}
	return out
}
