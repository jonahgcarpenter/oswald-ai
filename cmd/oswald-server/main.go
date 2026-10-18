// Command oswald-server runs the Oswald AI service. It owns configuration
// selection and process exit; configuration loading, the banner, signals, and
// runtime composition live in internal/app/startup.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"

	"github.com/jonahgcarpenter/oswald-ai/internal/app/startup"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

func main() {
	fs := flag.NewFlagSet("oswald-server", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		_, _ = io.WriteString(os.Stderr, "usage: oswald-server [--root DIR]\n")
	}
	root := fs.String("root", config.OswaldHomeDir, "configuration root directory")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		os.Exit(2)
	}
	if err := startup.Serve(context.Background(), *root, os.Stdout); err != nil {
		os.Exit(1)
	}
}
