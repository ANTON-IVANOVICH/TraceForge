package cli

import (
	"context"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"metrics-system/internal/cli/client"
	"metrics-system/internal/cli/output"
	"metrics-system/internal/model"
)

// defaultHeartbeat is the metric every agent reports, which is what makes it a
// usable liveness signal.
const defaultHeartbeat = "uptime_seconds"

// staleAfter is how long an agent may be silent before it is called stale.
const staleAfter = 2 * time.Minute

func newAgentsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "agents",
		Aliases: []string{"agent"},
		Short:   "Inspect the agents reporting to this server",
	}
	cmd.AddCommand(newAgentsListCmd())
	return group(cmd)
}

// agentRow is what `agents list` prints and what `-o json` encodes.
type agentRow struct {
	AgentID  string    `json:"agent_id"`
	Status   string    `json:"status"`
	LastSeen time.Time `json:"last_seen"`
	Uptime   float64   `json:"uptime_seconds"`
	Tenant   string    `json:"tenant,omitempty"`
}

func newAgentsListCmd() *cobra.Command {
	var (
		heartbeat string
		lookback  time.Duration
	)

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List agents seen recently",
		Long: `List the agents that have reported recently.

The server keeps no agent registry — an agent is simply whatever has been
writing metrics. This command therefore derives the list from a heartbeat metric
(uptime_seconds by default), grouping its series by the agent_id label. Point
--heartbeat at another metric if your agents do not report uptime.

Examples:
  metricsctl agents list
  metricsctl agents list --lookback 1h -o json
  metricsctl agents list -o name | xargs -n1 -I{} metricsctl query cpu_usage_percent -l agent_id={}`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := MustFromContext(cmd.Context())
			api, err := c.Client()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), c.Timeout)
			defer cancel()

			now := time.Now()
			metrics, err := api.Query(ctx, client.Query{Name: heartbeat, From: now.Add(-lookback), To: now})
			if err != nil {
				return wrapAPIError("list agents", err)
			}

			rows := collapseAgents(metrics, now)
			tbl := output.Table{Headers: []string{"agent-id", "status", "last-seen", "uptime", "tenant"}}
			for _, a := range rows {
				status := c.Color.Green(a.Status)
				if a.Status == "stale" {
					status = c.Color.Yellow(a.Status)
				}
				tenant := a.Tenant
				if tenant == "" {
					tenant = "-"
				}
				tbl.Rows = append(tbl.Rows, []string{
					a.AgentID,
					status,
					output.Age(a.LastSeen),
					output.Duration(time.Duration(a.Uptime) * time.Second),
					tenant,
				})
			}
			return c.Printer.Print(rows, tbl)
		},
	}

	f := cmd.Flags()
	f.StringVar(&heartbeat, "heartbeat", defaultHeartbeat, "metric used as the agent liveness signal")
	f.DurationVar(&lookback, "lookback", 15*time.Minute, "how far back to look for a heartbeat")
	return cmd
}

// collapseAgents reduces heartbeat points to the newest one per agent.
func collapseAgents(metrics []model.Metric, now time.Time) []agentRow {
	byAgent := make(map[string]agentRow, len(metrics))
	for _, m := range metrics {
		id := m.Labels["agent_id"]
		if id == "" {
			continue
		}
		if cur, ok := byAgent[id]; ok && !m.Timestamp.After(cur.LastSeen) {
			continue
		}
		byAgent[id] = agentRow{
			AgentID:  id,
			LastSeen: m.Timestamp,
			Uptime:   m.Value,
			Tenant:   m.Labels["tenant"],
		}
	}

	rows := make([]agentRow, 0, len(byAgent))
	for _, a := range byAgent {
		a.Status = "healthy"
		if now.Sub(a.LastSeen) > staleAfter {
			a.Status = "stale"
		}
		rows = append(rows, a)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].AgentID < rows[j].AgentID })
	return rows
}
