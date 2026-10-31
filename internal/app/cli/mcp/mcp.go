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
	case "list":
		return listCmd(ctx, root, args[1:], stdout, stderr)
	case "add":
		return add(ctx, root, args[1:], stdout, stderr)
	case "edit":
		return edit(ctx, root, args[1:], stdout, stderr)
	case "remove":
		return remove(ctx, root, args[1:], stdout, stderr)
	case "test":
		return testCmd(ctx, root, args[1:], stdout, stderr)
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
	fmt.Fprintln(w, "usage: oswald mcp [--profile NAME] <list|add|edit|remove|test|auth> [options]")
	fmt.Fprintln(w, "  list                  configured servers with transport, auth, and token state")
	fmt.Fprintln(w, "  add <server> (--url URL | --command CMD) [options]")
	fmt.Fprintln(w, "                        add a server through staged validation")
	fmt.Fprintln(w, "  edit <server> [options]")
	fmt.Fprintln(w, "                        merge fields over an existing server")
	fmt.Fprintln(w, "  remove <server> [--with-tokens]")
	fmt.Fprintln(w, "                        delete a server (tokens kept unless flagged)")
	fmt.Fprintln(w, "  test <server> [--timeout DURATION]")
	fmt.Fprintln(w, "                        dial once and list tools")
	fmt.Fprintln(w, "  auth <server> [--flow browser|device] [--no-browser] [--timeout DURATION]")
	fmt.Fprintln(w, "                        OAuth authorization flow")
}
