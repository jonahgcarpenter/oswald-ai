package mcp

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// remove deletes the mcp_servers.<name> entry through staged validation.
// The server directory is kept unless --full is passed.
func remove(ctx context.Context, root string, args []string, stdout, stderr io.Writer) int {
	_ = ctx
	fs := flag.NewFlagSet("oswald mcp remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := profileFlag(fs)
	full := fs.Bool("full", false, "also delete the server directory")
	flagArgs, rest := partitionArgs(args, map[string]bool{"profile": true}, map[string]bool{"full": true})
	if err := fs.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		fmt.Fprintln(stderr, "usage: oswald mcp remove <server> [--profile NAME] [--full]")
		return exitUsage
	}
	name := serverName(rest[0])
	profileName, err := config.ResolveProfile(*profile)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return exitUsage
	}
	// Resolve the profile root for optional directory cleanup before mutating.
	_, _, profileRoot, err := resolveScope(root, *profile, true)
	if err != nil {
		reportMCPError(stderr, err)
		return exitError
	}
	existed, err := config.RemoveMCPServer(root, profileName, name)
	if err != nil {
		reportMCPError(stderr, err)
		return exitError
	}
	if !existed {
		fmt.Fprintf(stderr, "oswald: unknown MCP server %q\n", name)
		return exitError
	}
	if *full {
		dropped, err := mcp.RemoveServerTokens(profileRoot, name)
		if err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
		if dropped {
			fmt.Fprintf(stdout, "removed MCP server %q and its directory\n", name)
		} else {
			fmt.Fprintf(stdout, "removed MCP server %q (no directory stored)\n", name)
		}
		return exitOK
	}
	fmt.Fprintf(stdout, "removed MCP server %q (directory kept)\n", name)
	return exitOK
}
