package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"metrics-system/internal/alerting/rules"
	"metrics-system/internal/alerting/silence"
	"metrics-system/internal/cli/client"
	"metrics-system/internal/cli/output"
)

func newSilencesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "silences",
		Aliases: []string{"silence"},
		Short:   "Manage alert silences",
	}
	cmd.AddCommand(newSilencesListCmd(), newSilencesCreateCmd(), newSilencesDeleteCmd())
	return group(cmd)
}

func completeSilenceIDs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	c := FromContext(cmd.Context())
	if c == nil {
		return nil, cobra.ShellCompDirectiveError
	}
	api, err := c.Client()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), time.Second)
	defer cancel()

	list, err := api.ListSilences(ctx)
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	var out []string
	for _, s := range list {
		if strings.HasPrefix(s.ID, toComplete) {
			out = append(out, s.ID+"\t"+matchersString(s.Matchers))
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

func newSilencesListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List silences",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := MustFromContext(cmd.Context())
			api, err := c.Client()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), c.Timeout)
			defer cancel()

			list, err := api.ListSilences(ctx)
			if err != nil {
				return wrapAPIError("list silences", err)
			}
			now := time.Now()
			if !all {
				active := make([]*silence.Silence, 0, len(list))
				for _, s := range list {
					if s.Active(now) || now.Before(s.StartsAt) {
						active = append(active, s)
					}
				}
				list = active
			}
			return c.Printer.Print(list, silencesTable(c, list, now))
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "include expired silences")
	return cmd
}

func silencesTable(c *Context, list []*silence.Silence, now time.Time) output.Table {
	tbl := output.Table{Headers: []string{"id", "status", "matchers", "ends-in", "created-by", "comment"}}
	for _, s := range list {
		status := c.Color.Grey("expired")
		endsIn := "-"
		switch {
		case s.Active(now):
			status = c.Color.Green("active")
			endsIn = output.Duration(s.EndsAt.Sub(now))
		case now.Before(s.StartsAt):
			status = c.Color.Yellow("pending")
			endsIn = output.Duration(s.EndsAt.Sub(now))
		}
		tbl.Rows = append(tbl.Rows, []string{
			s.ID,
			status,
			truncate(matchersString(s.Matchers), 48),
			endsIn,
			s.CreatedBy,
			truncate(s.Comment, 32),
		})
	}
	return tbl
}

func matchersString(ms []silence.Matcher) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		parts = append(parts, m.String())
	}
	return strings.Join(parts, ",")
}

func newSilencesCreateCmd() *cobra.Command {
	var (
		matchers  []string
		duration  time.Duration
		startsAt  string
		endsAt    string
		comment   string
		createdBy string
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Silence alerts matching a set of label matchers",
		Long: `Create a silence: matching alerts stop producing notifications while it is
active, but stay visible in "metricsctl alerts list".

Matchers use the same syntax as the rule DSL:
  agent_id=web-1        equality
  agent_id!=web-1       inequality
  env=~"prod.*"         regex (fully anchored)
  env!~staging          negated regex

Examples:
  metricsctl silences create -m agent_id=web-1 --duration 2h --comment "planned maintenance"
  metricsctl silences create -m 'env=~prod.*' -m severity=warning --duration 30m`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := MustFromContext(cmd.Context())
			if len(matchers) == 0 {
				return Usagef("at least one --matcher is required (a silence with none would mute everything)")
			}

			parsed := make([]silence.Matcher, 0, len(matchers))
			for _, raw := range matchers {
				m, err := silence.ParseMatcher(raw)
				if err != nil {
					return Usagef("matcher %q: %v", raw, err)
				}
				parsed = append(parsed, m)
			}

			spec := client.SilenceSpec{Matchers: parsed, Comment: comment, CreatedBy: createdBy}
			now := time.Now()
			if startsAt != "" {
				t, err := parseTime(startsAt, now)
				if err != nil {
					return &UsageError{Err: err}
				}
				spec.StartsAt = &t
			}
			switch {
			case endsAt != "":
				t, err := parseTime(endsAt, now)
				if err != nil {
					return &UsageError{Err: err}
				}
				spec.EndsAt = &t
			case duration > 0:
				d := rules.Duration(duration)
				spec.Duration = &d
			default:
				return Usagef("one of --duration or --ends-at is required")
			}

			api, err := c.Client()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), c.Timeout)
			defer cancel()

			created, err := api.CreateSilence(ctx, spec)
			if err != nil {
				return wrapAPIError("create silence", err)
			}
			if c.Printer.Format() == output.FormatTable {
				_, _ = fmt.Fprintf(c.Stdout, "%s silence/%s\n", c.Color.Green("created"), created.ID)
				return nil
			}
			return c.Printer.Print(created, silencesTable(c, []*silence.Silence{created}, now))
		},
	}

	f := cmd.Flags()
	f.StringArrayVarP(&matchers, "matcher", "m", nil, "label matcher, repeatable (key=value, key!=value, key=~regex, key!~regex)")
	f.DurationVar(&duration, "duration", 0, "how long the silence lasts from its start")
	f.StringVar(&startsAt, "starts-at", "", "when it begins (RFC3339 or a relative offset; default now)")
	f.StringVar(&endsAt, "ends-at", "", "when it ends (RFC3339 or a relative offset)")
	f.StringVar(&comment, "comment", "", "why this silence exists")
	f.StringVar(&createdBy, "created-by", "", "who created it")
	return cmd
}

func newSilencesDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "delete <id>...",
		Aliases:           []string{"rm", "expire"},
		Short:             "Delete silences",
		Args:              usageArgs(cobra.MinimumNArgs(1)),
		ValidArgsFunction: completeSilenceIDs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c := MustFromContext(cmd.Context())
			api, err := c.Client()
			if err != nil {
				return err
			}
			ok, err := confirm(c, fmt.Sprintf("Delete %d silence(s)?", len(args)))
			if err != nil {
				return err
			}
			if !ok {
				_, _ = fmt.Fprintln(c.Stderr, "Aborted.")
				return nil
			}
			for _, id := range args {
				ctx, cancel := context.WithTimeout(cmd.Context(), c.Timeout)
				err := api.DeleteSilence(ctx, id)
				cancel()
				if err != nil {
					return wrapAPIError("delete silence "+id, err)
				}
				_, _ = fmt.Fprintf(c.Stdout, "%s silence/%s\n", c.Color.Green("deleted"), id)
			}
			return nil
		},
	}
}
