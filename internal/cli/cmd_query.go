package cli

import (
	"context"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"metrics-system/internal/cli/client"
	"metrics-system/internal/cli/output"
)

func newQueryCmd() *cobra.Command {
	var (
		labels []string
		from   string
		to     string
		agg    string
		step   time.Duration
		limit  int
	)

	cmd := &cobra.Command{
		Use:     "query <metric>",
		Aliases: []string{"q"},
		Short:   "Query stored metrics",
		Long: `Query the metric store by name, optionally filtered by labels, windowed by
time and aggregated into steps.

Examples:
  # Raw points for a metric
  metricsctl query cpu_usage_percent

  # One host, last hour, averaged per minute
  metricsctl query cpu_usage_percent -l agent_id=web-1 --from -1h --agg avg --step 1m

  # The 99th percentile, as JSON for jq
  metricsctl query cpu_usage_percent --agg p99 --step 5m -o json | jq '.[].value'`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := MustFromContext(cmd.Context())

			filter, err := parseLabels(labels)
			if err != nil {
				return err
			}
			now := time.Now()
			fromTime, err := parseTime(from, now)
			if err != nil {
				return &UsageError{Err: err}
			}
			toTime, err := parseTime(to, now)
			if err != nil {
				return &UsageError{Err: err}
			}
			if step > 0 && agg == "" {
				return Usagef("--step requires --agg")
			}

			api, err := c.Client()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), c.Timeout)
			defer cancel()

			metrics, err := api.Query(ctx, client.Query{
				Name:   args[0],
				Labels: filter,
				From:   fromTime,
				To:     toTime,
				Agg:    agg,
				Step:   step,
				Limit:  limit,
			})
			if err != nil {
				return wrapAPIError("query", err)
			}

			tbl := output.Table{Headers: []string{"name", "value", "timestamp", "labels"}}
			for _, m := range metrics {
				tbl.Rows = append(tbl.Rows, []string{
					m.Name,
					strconv.FormatFloat(m.Value, 'f', -1, 64),
					m.Timestamp.Format(time.RFC3339),
					labelString(m.Labels),
				})
			}
			return c.Printer.Print(metrics, tbl)
		},
	}

	f := cmd.Flags()
	f.StringArrayVarP(&labels, "label", "l", nil, "label filter, repeatable (key=value)")
	f.StringVar(&from, "from", "", "start of the window: RFC3339 or a relative offset like -1h")
	f.StringVar(&to, "to", "", "end of the window: RFC3339 or a relative offset")
	f.StringVar(&agg, "agg", "", "aggregation: avg|min|max|sum|count|p50|p90|p95|p99")
	f.DurationVar(&step, "step", 0, "aggregation window size (requires --agg)")
	f.IntVar(&limit, "limit", 0, "maximum points to return")

	_ = cmd.RegisterFlagCompletionFunc("agg", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"avg", "min", "max", "sum", "count", "p50", "p90", "p95", "p99"}, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

func newStatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Show the server's pipeline and storage counters",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := MustFromContext(cmd.Context())
			api, err := c.Client()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), c.Timeout)
			defer cancel()

			stats, err := api.Stats(ctx)
			if err != nil {
				return wrapAPIError("stats", err)
			}
			tbl := output.Table{
				Headers: []string{"ingested", "stored", "dropped", "invalid", "series", "points"},
				Rows: [][]string{{
					strconv.FormatInt(stats.Pipeline.Ingested, 10),
					strconv.FormatInt(stats.Pipeline.Stored, 10),
					strconv.FormatInt(stats.Pipeline.Dropped, 10),
					strconv.FormatInt(stats.Pipeline.Invalid, 10),
					strconv.Itoa(stats.Storage.Series),
					strconv.FormatInt(stats.Storage.Points, 10),
				}},
			}
			return c.Printer.Print(stats, tbl)
		},
	}
}
