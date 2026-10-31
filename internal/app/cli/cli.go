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

	cliconfig "github.com/jonahgcarpenter/oswald-ai/internal/app/cli/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/cli/api"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/cli/model"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/cli/providers"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/cli/env"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/cli/gateway"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/cli/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/cli/profiles"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/cli/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/cli/setup"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/cli/start"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
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
	rootFlag := fs.String("root", "", "configuration root directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	root, err := config.ResolveGlobalRoot(*rootFlag)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return exitUsage
	}
	rest := fs.Args()
	if len(rest) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch rest[0] {
	case "setup":
		return setup.Run(ctx, root, rest[1:], stdout, stderr)
	case "start":
		return start.Run(ctx, root, stdout)
	case "profile":
		return profiles.Run(ctx, root, rest[1:], stdin, stdout, stderr)
	case "mcp":
		return mcp.Run(ctx, root, rest[1:], stdin, stdout, stderr)
	case "providers":
		return providers.Run(ctx, root, rest[1:], stdout, stderr)
	case "model":
		return model.Run(ctx, root, rest[1:], stdout, stderr)
	case "api":
		return api.Run(ctx, root, rest[1:], stdout, stderr)
	case "gateway":
		return gateway.Run(ctx, root, rest[1:], stdout, stderr)
	case "runtime":
		return runtime.Run(ctx, root, rest[1:], stdout, stderr)
	case "config":
		return cliconfig.Run(ctx, root, rest[1:], stdout, stderr)
	case "env":
		return env.Run(ctx, root, rest[1:], stdin, stdout, stderr)
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
  setup                  seed a first-install configuration root
  start                  run the oswald service
  profile create <name>  create a private operator profile
  profile delete <name>  delete an operator profile and its data
  mcp auth <server>      authorize a profile MCP server with OAuth
  mcp list|add|edit|remove|test
                         manage profile MCP servers
  gateway <name> <enable|disable>
                         enable or disable a gateway platform
  runtime show|set         read and safely modify runtime configuration
  providers list|show|add|remove
                         manage LLM providers
  model show|set         manage model selection
  api show|set-key|clear-key
                         manage the OpenAI gateway credential
  config show|get|set|unset|diff|undo|validate|reload
                         read and safely modify configuration
  env show|set|unset     manage the private profile environment
`)
}
