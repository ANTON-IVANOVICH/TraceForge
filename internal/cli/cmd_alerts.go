package cli

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"metrics-system/internal/alerting/rules"
	"metrics-system/internal/cli/output"
)

func newAlertsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "alerts",
		Aliases: []string{"alert"},
		Short:   "Inspect active alerts",
	}
	cmd.AddCommand(newAlertsListCmd())
	return group(cmd)
}

func newAlertsListCmd() *cobra.Command {
	var (
		watch    bool
		interval time.Duration
		state    string
	)

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List pending and firing alerts",
		Long: `List the alerts the server currently considers active.

"pending" means the condition holds but the rule's ` + "`for`" + ` window has not elapsed;
"firing" means it has.

Examples:
  metricsctl alerts list
  metricsctl alerts list --state firing -o json
  metricsctl alerts list --watch --interval 2s`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := MustFromContext(cmd.Context())
			if state != "" && state != "firing" && state != "pending" {
				return Usagef("--state must be firing or pending")
			}
			api, err := c.Client()
			if err != nil {
				return err
			}

			render := func() error {
				ctx, cancel := context.WithTimeout(cmd.Context(), c.Timeout)
				defer cancel()

				list, err := api.ListAlerts(ctx)
				if err != nil {
					return wrapAPIError("list alerts", err)
				}
				list = filterAlerts(list, state)
				sortAlerts(list)
				return c.Printer.Print(list, alertsTable(c, list))
			}

			if !watch {
				return render()
			}
			// Watch mode only makes sense on a terminal: the screen is cleared with
			// an ANSI escape between frames, which would be noise in a pipe.
			if c.Printer.Format() != output.FormatTable {
				return Usagef("--watch requires the table output format")
			}
			// time.NewTicker panics on a non-positive interval; a flag value must
			// never be able to panic the process.
			if interval <= 0 {
				return Usagef("--interval must be positive")
			}
			if err := render(); err != nil {
				return err
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-cmd.Context().Done():
					return nil // Ctrl+C: a clean exit, not an error
				case <-ticker.C:
					if output.IsTerminal(c.Stdout) {
						_, _ = fmt.Fprint(c.Stdout, "\033[2J\033[H") // clear screen, home cursor
					}
					if err := render(); err != nil {
						_, _ = fmt.Fprintln(c.Stderr, err)
					}
				}
			}
		},
	}

	f := cmd.Flags()
	f.BoolVarP(&watch, "watch", "w", false, "refresh continuously until interrupted")
	f.DurationVar(&interval, "interval", 5*time.Second, "refresh interval for --watch")
	f.StringVar(&state, "state", "", "filter by state: firing|pending")
	_ = cmd.RegisterFlagCompletionFunc("state", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"firing", "pending"}, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

func filterAlerts(list []*rules.AlertState, state string) []*rules.AlertState {
	if state == "" {
		return list
	}
	out := make([]*rules.AlertState, 0, len(list))
	for _, a := range list {
		if string(a.State) == state {
			out = append(out, a)
		}
	}
	return out
}

// sortAlerts puts what is on fire first, then orders deterministically.
func sortAlerts(list []*rules.AlertState) {
	rank := map[rules.State]int{rules.StateFiring: 0, rules.StatePending: 1}
	sort.SliceStable(list, func(i, j int) bool {
		if ri, rj := rank[list[i].State], rank[list[j].State]; ri != rj {
			return ri < rj
		}
		if list[i].Labels["alertname"] != list[j].Labels["alertname"] {
			return list[i].Labels["alertname"] < list[j].Labels["alertname"]
		}
		return list[i].Fingerprint < list[j].Fingerprint
	})
}

func alertsTable(c *Context, list []*rules.AlertState) output.Table {
	tbl := output.Table{Headers: []string{"alert", "state", "severity", "value", "active", "labels"}}
	for _, a := range list {
		tbl.Rows = append(tbl.Rows, []string{
			a.Labels["alertname"],
			c.Color.State(string(a.State)),
			c.Color.Severity(a.Labels["severity"]),
			fmt.Sprintf("%g", a.Value),
			output.Age(a.ActiveAt),
			truncate(labelString(withoutKeys(a.Labels, "alertname", "severity")), 60),
		})
	}
	return tbl
}

func withoutKeys(labels map[string]string, drop ...string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	for _, k := range drop {
		delete(out, k)
	}
	return out
}
