// Package cli implements the oswald command-line interface. It owns argument
// dispatch, command groups, and exit-code mapping; the concrete binaries in
// cmd/ only call Run and translate its result into a process exit.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/jonahgcarpenter/oswald-ai/internal/cli/profiles"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// Exit codes are the shared CLI convention. Run returns one; only main exits.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// Run parses global flags, dispatches one command, and returns an exit code.
// stdin/stdout/stderr are injected so command behavior is testable off-terminal.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("oswald", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }
	root := fs.String("root", config.OswaldHomeDir, "configuration root directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	rest := fs.Args()
	if len(rest) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch rest[0] {
	case "run":
		return run(ctx, *root, stdout)
	case "profile":
		return profiles.Run(ctx, *root, rest[1:], stdin, stdout, stderr)
	case "help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown command %q\n", rest[0])
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: oswald [--root DIR] <command> [arguments]

commands:
  run                    run the oswald service
  profile create <name>  create a private operator profile
  profile delete <name>  delete an operator profile and its data
`)
}
