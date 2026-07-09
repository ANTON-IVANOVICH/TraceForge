// Command metricsctl is the command-line client for a TraceForge server.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"metrics-system/internal/cli"
)

// version is injected at build time with -ldflags "-X main.version=v0.8.0".
var version = "dev"

func main() {
	// A cancelled context lets long-running commands (`alerts list --watch`) stop
	// on Ctrl+C instead of being killed mid-write.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	root := cli.NewRootCmd(version, nil)

	if err := root.ExecuteContext(ctx); err != nil {
		// An interrupted command is not a failure to report; the shell already
		// knows what happened from the signal.
		if errors.Is(err, context.Canceled) {
			os.Exit(cli.ExitOK)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(cli.ExitCode(err))
	}
}
