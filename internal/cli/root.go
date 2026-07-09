// Package cli builds metricsctl: a kubectl-shaped command-line client for a
// TraceForge server. The shape is borrowed on purpose — `noun verb`, persistent
// flags, `-o json`, contexts, shell completion — because a CLI is a UI, and a
// familiar one is one people actually use.
package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"metrics-system/internal/cli/client"
	"metrics-system/internal/cli/config"
	"metrics-system/internal/cli/output"
)

// Options are the root command's persistent flags, shared by every subcommand.
type Options struct {
	ConfigPath  string
	ContextName string
	Output      string
	Server      string
	APIKey      string
	Token       string
	Insecure    bool
	Timeout     time.Duration
	Verbose     bool
	NoColor     bool
	AssumeYes   bool

	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
}

// NewRootCmd assembles the command tree.
func NewRootCmd(version string, opts *Options) *cobra.Command {
	if opts == nil {
		opts = &Options{}
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}

	cmd := &cobra.Command{
		Use:   "metricsctl",
		Short: "Command-line client for a TraceForge metrics server",
		Long: `metricsctl manages and inspects a TraceForge server: query metrics,
manage alerting rules and silences, and watch active alerts.

Every command supports -o table|json|yaml|name, so the CLI composes with jq,
xargs and the rest of the shell.

Examples:
  # Query the last hour of CPU, averaged per minute
  metricsctl query cpu_usage_percent --from -1h --agg avg --step 1m

  # Apply alerting rules declaratively, previewing first
  metricsctl rules apply -f rules.yaml --dry-run
  metricsctl rules apply -f rules.yaml

  # What is on fire?
  metricsctl alerts list --watch

  # Mute a host during maintenance
  metricsctl silences create -m agent_id=web-1 --duration 2h --comment "planned"`,

		// Cobra otherwise prints the error *and* a wall of usage text on every
		// failure, including failures that have nothing to do with usage.
		SilenceErrors:     true,
		SilenceUsage:      true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error { return setup(cmd, opts) },

		// Without these, `metricsctl bogus` returns Cobra's plain "unknown
		// command" error, which exits 1 rather than the contractual usage code 2.
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	// A flag error is a usage error: mark it so main exits with code 2 and prints
	// the usage that would actually help.
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		_ = c.Usage()
		return &UsageError{Err: err}
	})

	f := cmd.PersistentFlags()
	f.StringVar(&opts.ConfigPath, "config", "", "config file (default "+config.DefaultPath()+")")
	f.StringVar(&opts.ContextName, "context", "", "context to use from the config file")
	f.StringVarP(&opts.Output, "output", "o", "table", "output format: "+strings.Join(output.Formats(), "|"))
	f.StringVar(&opts.Server, "server", "", "server URL, overriding the context")
	f.StringVar(&opts.APIKey, "api-key", "", "API key, overriding the context")
	f.StringVar(&opts.Token, "token", "", "bearer token, overriding the context")
	f.BoolVar(&opts.Insecure, "insecure", false, "skip TLS certificate verification")
	f.DurationVar(&opts.Timeout, "timeout", 30*time.Second, "per-request timeout")
	f.BoolVarP(&opts.Verbose, "verbose", "v", false, "verbose output on stderr")
	f.BoolVar(&opts.NoColor, "no-color", false, "disable coloured output")
	f.BoolVarP(&opts.AssumeYes, "yes", "y", false, "assume yes to confirmation prompts")

	_ = cmd.RegisterFlagCompletionFunc("output",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return output.Formats(), cobra.ShellCompDirectiveNoFileComp
		})
	_ = cmd.RegisterFlagCompletionFunc("context",
		func(c *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			cfg, err := config.Load(opts.ConfigPath)
			if err != nil {
				return nil, cobra.ShellCompDirectiveError
			}
			return cfg.Names(), cobra.ShellCompDirectiveNoFileComp
		})

	cmd.AddCommand(
		newQueryCmd(),
		newRulesCmd(),
		newAlertsCmd(),
		newSilencesCmd(),
		newAgentsCmd(),
		newStatsCmd(),
		newConfigCmd(),
		newCompletionCmd(),
		newVersionCmd(version),
	)
	return cmd
}

// setup runs before every subcommand: load the config, resolve the context,
// apply flag overrides, build the printer. The API client is built lazily, so
// `metricsctl config get-contexts` needs no reachable server.
func setup(cmd *cobra.Command, opts *Options) error {
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return err
	}
	if config.InsecurePermissions(opts.ConfigPath) {
		_, _ = fmt.Fprintf(opts.Stderr, "warning: %s is readable by other users; it may hold credentials (chmod 600)\n",
			configPathOrDefault(opts.ConfigPath))
	}

	name, serverCtx, err := cfg.Resolve(opts.ContextName)
	if err != nil {
		return &NotFoundError{Err: err}
	}

	// Flags override the context, so a one-off invocation needs no config edit.
	effective := *serverCtx
	if opts.Server != "" {
		effective.Server = opts.Server
	}
	if opts.APIKey != "" && opts.Token != "" {
		return Usagef("--api-key and --token are mutually exclusive")
	}
	// A credential flag *replaces* the context's credential rather than adding to
	// it. Merging them would send the context's API key alongside the flag's
	// bearer token — to whatever `--server` now points at.
	switch {
	case opts.APIKey != "":
		effective.Auth = config.Auth{APIKey: opts.APIKey}
	case opts.Token != "":
		effective.Auth = config.Auth{Token: opts.Token}
	}
	if opts.Insecure {
		effective.Insecure = true
	}

	printer, err := output.NewPrinter(opts.Output, opts.Stdout)
	if err != nil {
		return &UsageError{Err: err}
	}

	c := &Context{
		Config:      cfg,
		ConfigPath:  opts.ConfigPath,
		ContextName: name,
		Server:      &effective,
		Printer:     printer,
		Color:       output.NewColorizer(opts.Stdout, opts.NoColor),
		Stdout:      opts.Stdout,
		Stderr:      opts.Stderr,
		Stdin:       opts.Stdin,
		Timeout:     opts.Timeout,
		Verbose:     opts.Verbose,
		AssumeYes:   opts.AssumeYes,
	}
	c.Debugf("using context %q at %s", name, effective.Server)

	cmd.SetContext(WithContext(cmd.Context(), c))
	return nil
}

func configPathOrDefault(p string) string {
	if p == "" {
		return config.DefaultPath()
	}
	return p
}

// wrapAPIError maps a server response onto the CLI's error classes, so exit
// codes stay meaningful: 3 for auth, 4 for not-found.
func wrapAPIError(action string, err error) error {
	if err == nil {
		return nil
	}
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		return fmt.Errorf("%s: %w", action, err)
	}
	switch apiErr.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return &AuthError{Err: fmt.Errorf("%s: %s", action, apiErr.Message)}
	case http.StatusNotFound:
		return &NotFoundError{Err: fmt.Errorf("%s: not found", action)}
	default:
		return fmt.Errorf("%s: %w", action, err)
	}
}
