// Command oswald is the single Oswald AI binary: a service runner plus
// operator commands. It delegates all logic to internal/app/cli and only owns
// process exit.
package main

import (
	"context"
	"os"

	"github.com/jonahgcarpenter/oswald-ai/internal/app/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
