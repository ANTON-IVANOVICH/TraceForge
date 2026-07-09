package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"metrics-system/internal/cli/config"
	"metrics-system/internal/cli/output"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage metricsctl contexts",
		Long: `A context names a server and the credential used against it, so one binary can
address production and staging without editing anything in between.

The config lives at ` + config.DefaultPath() + ` (override with --config or
METRICSCTL_CONFIG) and is written with 0600 permissions, because it holds
credentials.`,
	}
	cmd.AddCommand(newConfigGetContextsCmd(), newConfigCurrentContextCmd(),
		newConfigUseContextCmd(), newConfigSetContextCmd(), newConfigDeleteContextCmd(), newConfigViewCmd())
	return group(cmd)
}

func completeContextNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	c := FromContext(cmd.Context())
	if c == nil {
		return nil, cobra.ShellCompDirectiveError
	}
	return c.Config.Names(), cobra.ShellCompDirectiveNoFileComp
}

func newConfigGetContextsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "get-contexts",
		Aliases: []string{"contexts"},
		Short:   "List the configured contexts",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := MustFromContext(cmd.Context())

			tbl := output.Table{Headers: []string{"name", "current", "server", "auth"}}
			for _, name := range c.Config.Names() {
				ctx := c.Config.Contexts[name]
				current := ""
				if name == c.Config.CurrentContext {
					current = "*"
				}
				tbl.Rows = append(tbl.Rows, []string{name, current, ctx.Server, authKind(ctx)})
			}
			// -o name must print the context names, and the first column is the name.
			return c.Printer.Print(c.Config.Contexts, tbl)
		},
	}
}

// authKind describes a context's credential without ever printing it.
func authKind(ctx *config.Context) string {
	switch {
	case ctx.Auth.APIKey != "":
		return "api-key"
	case ctx.Auth.TokenFile != "":
		return "token-file"
	case ctx.Auth.Token != "":
		return "token"
	default:
		return "none"
	}
}

func newConfigCurrentContextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "current-context",
		Short: "Print the current context's name",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := MustFromContext(cmd.Context())
			_, err := fmt.Fprintln(c.Stdout, c.Config.CurrentContext)
			return err
		},
	}
}

func newConfigUseContextCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "use-context <name>",
		Short:             "Switch the current context",
		Args:              usageArgs(cobra.ExactArgs(1)),
		ValidArgsFunction: completeContextNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			c := MustFromContext(cmd.Context())
			if _, ok := c.Config.Contexts[args[0]]; !ok {
				return NotFoundf("context %q not found", args[0])
			}
			c.Config.CurrentContext = args[0]
			if err := c.Config.Save(c.ConfigPath); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(c.Stdout, "Switched to context %q.\n", args[0])
			return nil
		},
	}
}

func newConfigSetContextCmd() *cobra.Command {
	var (
		server    string
		apiKey    string
		token     string
		tokenFile string
		insecure  bool
		caFile    string
		use       bool
	)

	cmd := &cobra.Command{
		Use:   "set-context <name>",
		Short: "Create or update a context",
		Long: `Create or update a context.

With no flags on a terminal the command prompts for the missing values. In a
script pass the flags: a prompt that nobody can answer would hang the pipeline.

Examples:
  metricsctl config set-context prod --server https://metrics.example.com --token-file ~/.metricsctl/prod.token
  metricsctl config set-context local --server http://localhost:8080 --use`,
		Args:              usageArgs(cobra.ExactArgs(1)),
		ValidArgsFunction: completeContextNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			c := MustFromContext(cmd.Context())
			name := args[0]

			if countSet(apiKey, token, tokenFile) > 1 {
				return Usagef("--api-key, --token and --token-file are mutually exclusive")
			}

			ctx := c.Config.Contexts[name]
			if ctx == nil {
				ctx = &config.Context{}
			}

			if server == "" && ctx.Server == "" {
				if !interactive(c) {
					return Usagef("--server is required when there is no terminal to prompt on")
				}
				answer, err := prompt(c, fmt.Sprintf("Server URL for context %q", name), config.DefaultServer)
				if err != nil {
					return err
				}
				server = answer
			}
			if server != "" {
				ctx.Server = server
			}
			if apiKey != "" {
				ctx.Auth = config.Auth{APIKey: apiKey}
			}
			if token != "" {
				ctx.Auth = config.Auth{Token: token}
			}
			if tokenFile != "" {
				ctx.Auth = config.Auth{TokenFile: tokenFile}
			}
			if cmd.Flags().Changed("insecure") {
				ctx.Insecure = insecure
			}
			if caFile != "" {
				ctx.CAFile = caFile
			}

			if c.Config.Contexts == nil {
				c.Config.Contexts = map[string]*config.Context{}
			}
			c.Config.Contexts[name] = ctx
			if use || c.Config.CurrentContext == "" {
				c.Config.CurrentContext = name
			}
			if err := c.Config.Validate(); err != nil {
				return err
			}
			if err := c.Config.Save(c.ConfigPath); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(c.Stdout, "Context %q saved to %s.\n", name, configPathOrDefault(c.ConfigPath))
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&server, "server", "", "server URL, e.g. https://metrics.example.com")
	f.StringVar(&apiKey, "api-key", "", "API key credential")
	f.StringVar(&token, "token", "", "bearer token credential")
	f.StringVar(&tokenFile, "token-file", "", "file holding a bearer token, read on each invocation")
	f.BoolVar(&insecure, "insecure", false, "skip TLS certificate verification")
	f.StringVar(&caFile, "ca-file", "", "PEM bundle used to verify the server")
	f.BoolVar(&use, "use", false, "also make this the current context")
	_ = cmd.MarkFlagFilename("ca-file", "pem", "crt")
	_ = cmd.MarkFlagFilename("token-file")
	return cmd
}

func countSet(values ...string) int {
	var n int
	for _, v := range values {
		if v != "" {
			n++
		}
	}
	return n
}

func newConfigDeleteContextCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "delete-context <name>",
		Short:             "Remove a context",
		Args:              usageArgs(cobra.ExactArgs(1)),
		ValidArgsFunction: completeContextNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			c := MustFromContext(cmd.Context())
			name := args[0]
			if _, ok := c.Config.Contexts[name]; !ok {
				return NotFoundf("context %q not found", name)
			}
			if len(c.Config.Contexts) == 1 {
				return Usagef("refusing to delete the only context")
			}
			delete(c.Config.Contexts, name)
			if c.Config.CurrentContext == name {
				c.Config.CurrentContext = c.Config.Names()[0]
				_, _ = fmt.Fprintf(c.Stderr, "Current context was deleted; switched to %q.\n", c.Config.CurrentContext)
			}
			if err := c.Config.Save(c.ConfigPath); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(c.Stdout, "Context %q deleted.\n", name)
			return nil
		},
	}
}

func newConfigViewCmd() *cobra.Command {
	var showSecrets bool
	cmd := &cobra.Command{
		Use:   "view",
		Short: "Print the merged configuration",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := MustFromContext(cmd.Context())

			// Redact by default: `metricsctl config view` ends up pasted into issues.
			view := *c.Config
			view.Contexts = make(map[string]*config.Context, len(c.Config.Contexts))
			for name, ctx := range c.Config.Contexts {
				cp := *ctx
				if !showSecrets {
					if cp.Auth.APIKey != "" {
						cp.Auth.APIKey = "REDACTED"
					}
					if cp.Auth.Token != "" {
						cp.Auth.Token = "REDACTED"
					}
				}
				view.Contexts[name] = &cp
			}
			return c.Printer.Print(view, output.Table{
				Headers: []string{"current-context", "contexts"},
				Rows:    [][]string{{view.CurrentContext, fmt.Sprint(len(view.Contexts))}},
			})
		},
	}
	cmd.Flags().BoolVar(&showSecrets, "show-secrets", false, "print credentials instead of REDACTED")
	return cmd
}
