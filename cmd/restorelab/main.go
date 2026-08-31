// Command restorelab proves that your backups can actually recover your
// services: it restores them into an isolated environment, boots them,
// validates them, measures the real recovery time, and cleans up.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/restorelab/restorelab/internal/cli"
)

func main() {
	// Ctrl-C cancels the run's context. The recovery engine deliberately keeps
	// cleanup on a detached context, so an interrupted drill still destroys the
	// temporary workload it created.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(cli.Execute(ctx))
}
