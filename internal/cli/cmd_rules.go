package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"metrics-system/internal/alerting/rules"
	"metrics-system/internal/cli/client"
	"metrics-system/internal/cli/output"
)

// maxManifestSize bounds a manifest read from a file or stdin.
const maxManifestSize = 4 << 20

func newRulesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "rules",
		Aliases: []string{"rule"},
		Short:   "Manage alerting rules",
	}
	cmd.AddCommand(newRulesListCmd(), newRulesGetCmd(), newRulesApplyCmd(), newRulesDeleteCmd(), newRulesPreviewCmd())
	return group(cmd)
}

// completeRuleIDs turns `metricsctl rules get <TAB>` into the real rule list.
// A completion must be fast, so it gets a short deadline of its own.
func completeRuleIDs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
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

	list, err := api.ListRules(ctx)
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	var out []string
	for _, r := range list {
		if strings.HasPrefix(r.ID, toComplete) {
			out = append(out, r.ID+"\t"+r.Name)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

func newRulesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List alerting rules",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := MustFromContext(cmd.Context())
			api, err := c.Client()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), c.Timeout)
			defer cancel()

			list, err := api.ListRules(ctx)
			if err != nil {
				return wrapAPIError("list rules", err)
			}
			return c.Printer.Print(list, rulesTable(c, list))
		},
	}
}

func rulesTable(c *Context, list []*rules.Rule) output.Table {
	tbl := output.Table{Headers: []string{"id", "name", "severity", "for", "interval", "enabled", "expression"}}
	for _, r := range list {
		enabled := "true"
		if !r.Enabled {
			enabled = c.Color.Grey("false")
		}
		tbl.Rows = append(tbl.Rows, []string{
			r.ID,
			r.Name,
			c.Color.Severity(string(r.Severity)),
			r.For.String(),
			r.Interval.String(),
			enabled,
			truncate(r.Expression, 48),
		})
	}
	return tbl
}

func newRulesGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "get <id>",
		Short:             "Show one alerting rule",
		Args:              usageArgs(cobra.ExactArgs(1)),
		ValidArgsFunction: completeRuleIDs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c := MustFromContext(cmd.Context())
			api, err := c.Client()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), c.Timeout)
			defer cancel()

			rule, err := api.GetRule(ctx, args[0])
			if err != nil {
				return wrapAPIError("get rule "+args[0], err)
			}
			return c.Printer.Print(rule, rulesTable(c, []*rules.Rule{rule}))
		},
	}
}

func newRulesDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "delete <id>...",
		Aliases:           []string{"rm"},
		Short:             "Delete alerting rules",
		Args:              usageArgs(cobra.MinimumNArgs(1)),
		ValidArgsFunction: completeRuleIDs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c := MustFromContext(cmd.Context())
			api, err := c.Client()
			if err != nil {
				return err
			}
			ok, err := confirm(c, fmt.Sprintf("Delete %d rule(s)?", len(args)))
			if err != nil {
				return err
			}
			if !ok {
				_, _ = fmt.Fprintln(c.Stderr, "Aborted.")
				return nil
			}
			for _, id := range args {
				ctx, cancel := context.WithTimeout(cmd.Context(), c.Timeout)
				err := api.DeleteRule(ctx, id)
				cancel()
				if err != nil {
					return wrapAPIError("delete rule "+id, err)
				}
				_, _ = fmt.Fprintf(c.Stdout, "%s rule/%s\n", c.Color.Green("deleted"), id)
			}
			return nil
		},
	}
}

func newRulesPreviewCmd() *cobra.Command {
	var (
		from string
		to   string
		step time.Duration
	)
	cmd := &cobra.Command{
		Use:   "preview <expression>",
		Short: "Backtest an expression over historical data without saving it",
		Long: `Evaluate an expression at every step of a time window and report where it
would have matched. Use it to see what a rule would fire on before committing.

Examples:
  metricsctl rules preview 'cpu_usage_percent > 80' --from -1h --step 1m`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := MustFromContext(cmd.Context())
			api, err := c.Client()
			if err != nil {
				return err
			}
			now := time.Now()
			req := client.PreviewRequest{Expression: args[0]}
			if from != "" {
				t, err := parseTime(from, now)
				if err != nil {
					return &UsageError{Err: err}
				}
				req.From = &t
			}
			if to != "" {
				t, err := parseTime(to, now)
				if err != nil {
					return &UsageError{Err: err}
				}
				req.To = &t
			}
			if step > 0 {
				d := rules.Duration(step)
				req.Step = &d
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), c.Timeout)
			defer cancel()
			resp, err := api.PreviewRule(ctx, req)
			if err != nil {
				return wrapAPIError("preview", err)
			}

			tbl := output.Table{Headers: []string{"at", "samples", "labels"}}
			for _, r := range resp.Results {
				for _, s := range r.Samples {
					tbl.Rows = append(tbl.Rows, []string{
						r.At.Format(time.RFC3339),
						fmt.Sprintf("%g", s.Value),
						labelString(s.Labels),
					})
				}
			}
			return c.Printer.Print(resp, tbl)
		},
	}
	f := cmd.Flags()
	f.StringVar(&from, "from", "", "start of the window (RFC3339 or -1h)")
	f.StringVar(&to, "to", "", "end of the window")
	f.DurationVar(&step, "step", 0, "evaluation step (default 1m)")
	return cmd
}

// ---------------------------------------------------------------------------
// apply
// ---------------------------------------------------------------------------

// manifest is one document of a rules file. The apiVersion/kind envelope is the
// kubectl shape: it lets the same file later carry other kinds without ambiguity.
type manifest struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Expression  string            `yaml:"expression"`
		For         string            `yaml:"for"`
		Interval    string            `yaml:"interval"`
		Severity    string            `yaml:"severity"`
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
		Receivers   []string          `yaml:"receivers"`
		Enabled     *bool             `yaml:"enabled"`
	} `yaml:"spec"`
}

func newRulesApplyCmd() *cobra.Command {
	var (
		filename string
		dryRun   bool
	)
	cmd := &cobra.Command{
		Use:   "apply -f <file>",
		Short: "Create or update rules from a YAML file",
		Long: `Apply alerting rules declaratively. The file may hold several documents
separated by '---'. Each rule is created if new, updated if changed, and left
alone otherwise — so applying the same file twice is a no-op.

The rule's metadata.name doubles as its stable id, which is what makes apply
idempotent.

The manifest is the desired state in full: a field it omits is reconciled back to
the server's default (interval 15s, severity warning, enabled true, and any "for"
clause the expression itself carries), not left as it happens to be.

Examples:
  metricsctl rules apply -f rules.yaml
  metricsctl rules apply -f rules.yaml --dry-run
  cat rules.yaml | metricsctl rules apply -f -`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := MustFromContext(cmd.Context())
			// Checked here rather than with MarkFlagRequired, whose error bypasses
			// SetFlagErrorFunc and would exit 1 instead of 2.
			if filename == "" {
				return Usagef("-f/--filename is required (use - to read stdin)")
			}
			api, err := c.Client()
			if err != nil {
				return err
			}
			data, err := readManifest(c, filename)
			if err != nil {
				return err
			}
			manifests, err := parseManifests(data)
			if err != nil {
				return &UsageError{Err: err}
			}
			if len(manifests) == 0 {
				return Usagef("%s contains no rules", filename)
			}

			suffix := ""
			if dryRun {
				suffix = " (dry run)"
			}
			var failures []error
			for _, m := range manifests {
				action, err := applyRule(cmd.Context(), c, api, m, dryRun)
				if err != nil {
					failures = append(failures, fmt.Errorf("rule/%s: %w", m.Metadata.Name, err))
					continue
				}
				colour := c.Color.Green
				if action == "unchanged" {
					colour = c.Color.Grey
				}
				_, _ = fmt.Fprintf(c.Stdout, "%s rule/%s%s\n", colour(action), m.Metadata.Name, suffix)
			}
			if len(failures) > 0 {
				// Joined rather than flattened into a generic error: errors.As can
				// still find the AuthError or NotFoundError inside, so a 401 during
				// apply exits 3 and a 404 exits 4, per the exit-code contract.
				return errors.Join(failures...)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&filename, "filename", "f", "", "file to apply, or - for stdin")
	f.BoolVar(&dryRun, "dry-run", false, "validate against the server without persisting")
	_ = cmd.MarkFlagFilename("filename", "yaml", "yml", "json")
	return cmd
}

func readManifest(c *Context, filename string) ([]byte, error) {
	var r io.Reader
	if filename == "-" {
		r = c.Stdin
	} else {
		f, err := os.Open(filename)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", filename, err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, maxManifestSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxManifestSize {
		return nil, fmt.Errorf("manifest exceeds %d bytes", maxManifestSize)
	}
	return data, nil
}

// parseManifests decodes a multi-document YAML stream.
func parseManifests(data []byte) ([]*manifest, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	var out []*manifest
	seen := make(map[string]int)

	for i := 0; ; i++ {
		var m manifest
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		if m.Kind == "" && m.Metadata.Name == "" && m.Spec.Expression == "" {
			continue // an empty document between --- separators
		}
		if err := m.validate(i); err != nil {
			return nil, err
		}
		// The name is the rule's id. Two documents claiming it would both be
		// applied, and the last would silently win.
		if first, dup := seen[m.Metadata.Name]; dup {
			return nil, fmt.Errorf("document %d: duplicate metadata.name %q (already declared by document %d)",
				i, m.Metadata.Name, first)
		}
		seen[m.Metadata.Name] = i
		out = append(out, &m)
	}
}

func (m *manifest) validate(i int) error {
	if m.Kind != "Rule" {
		return fmt.Errorf("document %d: kind must be \"Rule\", got %q", i, m.Kind)
	}
	if m.APIVersion != "" && m.APIVersion != "v1" {
		return fmt.Errorf("document %d: unsupported apiVersion %q", i, m.APIVersion)
	}
	if strings.TrimSpace(m.Metadata.Name) == "" {
		return fmt.Errorf("document %d: metadata.name is required", i)
	}
	if strings.TrimSpace(m.Spec.Expression) == "" {
		return fmt.Errorf("document %d (%s): spec.expression is required", i, m.Metadata.Name)
	}
	return nil
}

// spec turns a manifest into the API's request body.
func (m *manifest) spec() (client.RuleSpec, error) {
	s := client.RuleSpec{
		ID:          m.Metadata.Name,
		Name:        m.Metadata.Name,
		Expression:  m.Spec.Expression,
		Severity:    m.Spec.Severity,
		Labels:      m.Spec.Labels,
		Annotations: m.Spec.Annotations,
		Receivers:   m.Spec.Receivers,
		Enabled:     m.Spec.Enabled,
	}
	if m.Spec.For != "" {
		d, err := time.ParseDuration(m.Spec.For)
		if err != nil {
			return s, fmt.Errorf("spec.for: %w", err)
		}
		dur := rules.Duration(d)
		s.For = &dur
	}
	if m.Spec.Interval != "" {
		d, err := time.ParseDuration(m.Spec.Interval)
		if err != nil {
			return s, fmt.Errorf("spec.interval: %w", err)
		}
		dur := rules.Duration(d)
		s.Interval = &dur
	}
	return s, nil
}

// applyRule reconciles one manifest against the server and reports what it did.
func applyRule(parent context.Context, c *Context, api *client.Client, m *manifest, dryRun bool) (string, error) {
	spec, err := m.spec()
	if err != nil {
		return "", err
	}
	// Compile locally before touching the server: it is the same code the server
	// runs, so a bad expression, severity or `for` is caught here — which is what
	// makes --dry-run mean something on the update path too, where the server is
	// never asked.
	desired, err := desiredRule(spec)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(parent, c.Timeout)
	defer cancel()

	existing, err := api.GetRule(ctx, m.Metadata.Name)
	switch {
	case err == nil:
		if sameRule(existing, desired) {
			return "unchanged", nil
		}
		if dryRun {
			return "updated", nil
		}
		if _, err := api.UpdateRule(ctx, m.Metadata.Name, spec); err != nil {
			return "", wrapAPIError("update", err)
		}
		return "updated", nil

	case client.Status(err) == http.StatusNotFound:
		if dryRun {
			return "created", nil
		}
		if _, err := api.CreateRule(ctx, spec); err != nil {
			return "", wrapAPIError("create", err)
		}
		return "created", nil

	default:
		return "", wrapAPIError("get", err)
	}
}

// desiredRule resolves the manifest into the rule the server would store: the
// same defaults (interval, severity, enabled) and the same `for` clause lifted
// out of the expression. Comparing against *that* is what lets an omitted field
// mean "reset to the default" without making every apply report an update.
func desiredRule(spec client.RuleSpec) (*rules.Rule, error) {
	r := &rules.Rule{
		Name:        spec.Name,
		Expression:  spec.Expression,
		Severity:    rules.Severity(spec.Severity),
		Labels:      spec.Labels,
		Annotations: spec.Annotations,
		Receivers:   spec.Receivers,
		Enabled:     spec.Enabled == nil || *spec.Enabled, // an omitted `enabled` means yes
	}
	if spec.For != nil {
		r.For = *spec.For
	}
	if spec.Interval != nil {
		r.Interval = *spec.Interval
	}
	if err := r.Compile(); err != nil {
		return nil, err
	}
	return r, nil
}

// sameRule reports whether the server's rule already matches the desired one, so
// a re-apply prints "unchanged" instead of pretending to have done work.
func sameRule(existing, desired *rules.Rule) bool {
	return existing.Name == desired.Name &&
		existing.Expression == desired.Expression &&
		existing.For == desired.For &&
		existing.Interval == desired.Interval &&
		existing.Severity == desired.Severity &&
		existing.Enabled == desired.Enabled &&
		equalLabels(existing.Labels, desired.Labels) &&
		equalLabels(existing.Annotations, desired.Annotations) &&
		equalStrings(existing.Receivers, desired.Receivers)
}

func equalLabels(a, b map[string]string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
