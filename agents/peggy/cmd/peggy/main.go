// Command peggy is the CLI entry point for the personal-assistant
// agent. See the agents/peggy package doc and README for details.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/erain/glue/agents/peggy"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(peggy.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
