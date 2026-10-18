// Package mcp implements the `oswald mcp` command group: explicit,
// operator-driven OAuth authorization for profile MCP servers.
package mcp

import (
	"context"
	"fmt"
	"io"
)

// Exit codes mirror the parent cli convention without importing it.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// Run dispatches the mcp command group.
func Run(ctx context.Context, root string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		mcpUsage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "auth":
		return auth(ctx, root, args[1:], stdin, stdout, stderr)
	case "help", "-h", "--help":
		mcpUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown mcp command %q\n", args[0])
		mcpUsage(stderr)
		return exitUsage
	}
}

func mcpUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald mcp auth <server> [--profile NAME] [--flow browser|device] [--no-browser] [--timeout DURATION]")
}
