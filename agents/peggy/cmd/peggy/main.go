// Command peggy is the CLI entry point for the personal-assistant
// agent. See the agents/peggy package doc and README for details.
package main

import (
	"context"
	"os"

	"github.com/erain/glue/agents/peggy"
)

func main() {
	os.Exit(peggy.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
