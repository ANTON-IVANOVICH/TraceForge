package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// Exit codes are part of a CLI's contract: `metricsctl rules get foo || handle`
// only works if the code says *why* it failed. 0 success, 1 generic, 2 usage,
// 3 authentication/authorization, 4 not found. Anything above 128 means the
// process was terminated by a signal, which the shell reports for us.
const (
	ExitOK       = 0
	ExitError    = 1
	ExitUsage    = 2
	ExitAuth     = 3
	ExitNotFound = 4
)

// UsageError marks a mistake in how the command was invoked (bad flag, wrong
// argument count). It is the one error class for which printing usage helps.
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

// Usagef builds a UsageError.
func Usagef(format string, args ...any) error {
	return &UsageError{Err: fmt.Errorf(format, args...)}
}

// usageArgs wraps a Cobra positional-argument validator so that "accepts 1
// arg(s), received 0" exits 2 like every other usage mistake. Cobra returns
// those as plain errors, and they never pass through SetFlagErrorFunc.
func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			return &UsageError{Err: err}
		}
		return nil
	}
}

// group turns a command that only carries subcommands into one that behaves.
//
// A Cobra command with no Run and no Args treats a mistyped subcommand as a
// positional argument: `metricsctl rules deletee x` prints the help text and
// exits 0, so `metricsctl rules deletee x && echo done` reports success while
// nothing happened. Giving it a Run makes NoArgs apply, and the typo becomes the
// usage error it is.
func group(cmd *cobra.Command) *cobra.Command {
	cmd.Args = usageArgs(cobra.NoArgs)
	cmd.RunE = func(c *cobra.Command, _ []string) error { return c.Help() }
	return cmd
}

// AuthError is a 401 or 403 from the server.
type AuthError struct{ Err error }

func (e *AuthError) Error() string { return e.Err.Error() }
func (e *AuthError) Unwrap() error { return e.Err }

// NotFoundError is a 404 from the server, or a locally-resolved resource that
// does not exist (an unknown context, say).
type NotFoundError struct{ Err error }

func (e *NotFoundError) Error() string { return e.Err.Error() }
func (e *NotFoundError) Unwrap() error { return e.Err }

// NotFoundf builds a NotFoundError.
func NotFoundf(format string, args ...any) error {
	return &NotFoundError{Err: fmt.Errorf(format, args...)}
}

// ExitCode maps an error to the process exit status.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var usage *UsageError
	var auth *AuthError
	var notFound *NotFoundError
	switch {
	case errors.As(err, &usage):
		return ExitUsage
	case errors.As(err, &auth):
		return ExitAuth
	case errors.As(err, &notFound):
		return ExitNotFound
	default:
		return ExitError
	}
}
