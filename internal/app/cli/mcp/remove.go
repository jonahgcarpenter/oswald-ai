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
// Token material is kept unless --with-tokens is passed; re-auth recreates it.
func remove(ctx context.Context, root string, args []string, stdout, stderr io.Writer) int {
	_ = ctx
	fs := flag.NewFlagSet("oswald mcp remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := profileFlag(fs)
	withTokens := fs.Bool("with-tokens", false, "also delete the server token directory")
	flagArgs, rest := partitionArgs(args, map[string]bool{"profile": true}, map[string]bool{"with-tokens": true})
	if err := fs.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		fmt.Fprintln(stderr, "usage: oswald mcp remove <server> [--profile NAME] [--with-tokens]")
		return exitUsage
	}
	name := serverName(rest[0])
	profileName, err := config.ResolveProfile(*profile)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return exitUsage
	}
	// Resolve the profile root for optional token cleanup before mutating.
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
	if *withTokens {
		dropped, err := mcp.RemoveServerTokens(profileRoot, name)
		if err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
		if dropped {
			fmt.Fprintf(stdout, "removed MCP server %q and its tokens\n", name)
		} else {
			fmt.Fprintf(stdout, "removed MCP server %q (no tokens stored)\n", name)
		}
		return exitOK
	}
	fmt.Fprintf(stdout, "removed MCP server %q (tokens kept)\n", name)
	return exitOK
}
