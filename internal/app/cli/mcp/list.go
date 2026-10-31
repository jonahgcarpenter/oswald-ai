package mcp

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"

	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// list prints one line per configured MCP server: name, transport kind,
// auth mode, and status. It never dials servers and never prints endpoints,
// headers, or secrets — names identify entries.
func listCmd(ctx context.Context, root string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("oswald mcp list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := profileFlag(fs)
	flagArgs, rest := partitionArgs(args, map[string]bool{"profile": true}, nil)
	if err := fs.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 0 {
		fmt.Fprintln(stderr, "usage: oswald mcp list [--profile NAME]")
		return exitUsage
	}
	profileCfg, _, profileRoot, err := resolveScope(root, *profile, false)
	if err != nil {
		reportMCPError(stderr, err)
		return exitError
	}
	servers := profileCfg.MCPServers
	if len(servers) == 0 {
		fmt.Fprintln(stdout, "no MCP servers")
		return exitOK
	}
	byServer := make(map[string]config.MCPServer, len(servers))
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		byServer[server.Name] = server
		names = append(names, server.Name)
	}
	sort.Strings(names)
	needAuthHint := false
	for _, name := range names {
		entry := byServer[name]
		transport := entry.Transport
		if transport == "" {
			transport = "stdio"
		}
		auth := "none"
		if entry.Auth == "oauth" {
			auth = "oauth"
		} else if len(entry.Headers) > 0 {
			auth = "headers"
		}
		status := "ok"
		if !entry.Enabled {
			status = "disabled"
		} else if entry.Auth == "oauth" {
			present, expired, err := mcp.TokenState(ctx, profileRoot, name)
			if err != nil {
				status = "unknown"
			} else if !present {
				status = "no-token"
				needAuthHint = true
			} else if expired {
				status = "expired"
				needAuthHint = true
			} else {
				status = "token"
			}
		}
		filters := ""
		if len(entry.Include) > 0 || len(entry.Exclude) > 0 {
			filters = fmt.Sprintf(" include=%d exclude=%d", len(entry.Include), len(entry.Exclude))
		}
		fmt.Fprintf(stdout, "%s  %s  %s  %s%s\n", name, transport, auth, status, filters)
	}
	if needAuthHint {
		fmt.Fprintln(stdout, `run "oswald mcp auth <server>" to authorize`)
	}
	return exitOK
}
