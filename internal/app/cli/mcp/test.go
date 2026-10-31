package mcp

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// maxTestTools bounds tool names printed by a successful test.
const maxTestTools = 20

// test dials one MCP server once and lists its tools. It exercises the same
// connection path as the runtime bridge; stdio entries spawn their
// operator-configured command. Nothing is persisted.
func testCmd(ctx context.Context, root string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("oswald mcp test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := profileFlag(fs)
	timeoutRaw := fs.String("timeout", "30s", "dial and ListTools bound")
	flagArgs, rest := partitionArgs(args, map[string]bool{"profile": true, "timeout": true}, nil)
	if err := fs.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		fmt.Fprintln(stderr, "usage: oswald mcp test <server> [--profile NAME] [--timeout DURATION]")
		return exitUsage
	}
	timeout, err := time.ParseDuration(strings.TrimSpace(*timeoutRaw))
	if err != nil || timeout <= 0 || timeout > 5*time.Minute {
		fmt.Fprintln(stderr, "oswald: invalid --timeout value")
		return exitUsage
	}
	name := serverName(rest[0])
	profileCfg, profileName, profileRoot, err := resolveScope(root, *profile, false)
	if err != nil {
		reportMCPError(stderr, err)
		return exitError
	}
	var server *config.MCPServer
	for i := range profileCfg.MCPServers {
		if profileCfg.MCPServers[i].Name == name {
			server = &profileCfg.MCPServers[i]
			break
		}
	}
	if server == nil {
		fmt.Fprintf(stderr, "oswald: unknown MCP server %q\n", name)
		return exitError
	}
	if !server.Enabled {
		fmt.Fprintf(stdout, "%s: disabled\n", name)
		return exitOK
	}
	manager, err := mcp.NewProfileManager(profileName, profileRoot, profileCfg.MCPServers, config.NewLogger(config.LevelError))
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	defer manager.Close()
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	specs, info, err := manager.ServerToolSpecs(dialCtx, profileName, name)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %s\n", config.SafeErrorText(err))
		return exitError
	}
	_ = info
	if len(specs) == 0 {
		fmt.Fprintf(stdout, "ok %s: 0 tools\n", name)
		return exitOK
	}
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	shown := names
	suffix := ""
	if len(names) > maxTestTools {
		shown = names[:maxTestTools]
		suffix = fmt.Sprintf(" (+%d more)", len(names)-maxTestTools)
	}
	fmt.Fprintf(stdout, "ok %s: %d tools: %s%s\n", name, len(names), strings.Join(shown, ", "), suffix)
	return exitOK
}
