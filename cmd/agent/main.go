// Command agent is a run-only compatibility shim for the oswald service. Use
// cmd/oswald for the full command-line interface.
package main

import (
	"context"
	"os"

	"github.com/jonahgcarpenter/oswald-ai/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), []string{"run"}, os.Stdin, os.Stdout, os.Stderr))
}
