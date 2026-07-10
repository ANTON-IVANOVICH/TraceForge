package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"metrics-system/internal/buildinfo"
	"metrics-system/internal/cli/output"
)

// newCompletionCmd emits the shell script that teaches a shell to complete
// metricsctl. Cobra generates the static half; the dynamic half (rule ids,
// silence ids, context names) comes from the ValidArgsFunction hooks and needs
// no extra work here.
func newCompletionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "completion <bash|zsh|fish|powershell>",
		Short: "Generate a shell completion script",
		Long: `Generate the completion script for a shell.

Bash:
  source <(metricsctl completion bash)                     # this session
  metricsctl completion bash > /etc/bash_completion.d/metricsctl

Zsh:
  metricsctl completion zsh > "${fpath[1]}/_metricsctl"

Fish:
  metricsctl completion fish > ~/.config/fish/completions/metricsctl.fish

PowerShell:
  metricsctl completion powershell | Out-String | Invoke-Expression

Completion is dynamic: "metricsctl rules get <TAB>" asks the server for the
rules that actually exist.`,
		Args:                  usageArgs(cobra.ExactArgs(1)),
		ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
		DisableFlagsInUseLine: true,

		// The completion script itself must be generatable without a server, and
		// without a config: skip the root's setup entirely.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },

		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			out := cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(out, true)
			case "zsh":
				return root.GenZshCompletion(out)
			case "fish":
				return root.GenFishCompletion(out, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(out)
			default:
				return Usagef("unsupported shell %q", args[0])
			}
		},
	}
}

// Version describes the build. It is buildinfo.Info under a name the CLI's users
// already know, and it gains the commit and the dirty flag: "which build is this"
// is a question a version string alone has never been able to answer.
type Version = buildinfo.Info

func newVersionCmd(build buildinfo.Info) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the metricsctl version",
		Args:  usageArgs(cobra.NoArgs),

		// Printing the version must work with no config and no server.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },

		RunE: func(cmd *cobra.Command, _ []string) error {
			// The root's PersistentPreRunE was skipped, so there is no printer;
			// honour -o by building one here against the command's own stream.
			format, _ := cmd.Root().PersistentFlags().GetString("output")
			printer, err := output.NewPrinter(format, cmd.OutOrStdout())
			if err != nil {
				return &UsageError{Err: err}
			}
			if printer.Format() == output.FormatTable {
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "metricsctl %s\n", build)
				return err
			}
			dirty := "false"
			if build.Dirty {
				dirty = "true"
			}
			return printer.Print(build, output.Table{
				Headers: []string{"version", "commit", "date", "dirty", "go", "platform"},
				Rows:    [][]string{{build.Version, build.Commit, build.Date, dirty, build.GoVersion, build.Platform}},
			})
		},
	}
}
