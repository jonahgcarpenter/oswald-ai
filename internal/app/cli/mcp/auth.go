package mcp

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
)

// auth runs the interactive OAuth authorization for one profile MCP server
// and persists the credential into the profile's private token store.
func auth(ctx context.Context, root string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("oswald mcp auth", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profileName := fs.String("profile", "default", "operator profile owning the MCP server")
	flow := fs.String("flow", "", "OAuth flow: browser or device (default from configuration)")
	noBrowser := fs.Bool("no-browser", false, "print the authorization URL and read a pasted response instead of opening a browser")
	timeoutRaw := fs.String("timeout", "", "authorization wait bound, e.g. 300s (default from configuration)")
	// Accept the server name in any position: Go's flag package stops at the
	// first positional, so partition flag tokens from positionals manually.
	// Known flags: --profile/--flow/--timeout take a value, --no-browser is
	// boolean; both --flag value and --flag=value forms are accepted.
	var flagArgs, rest []string
	for i := 0; i < len(args); i++ {
		token := args[i]
		name := token
		var inlineValue bool
		if j := strings.Index(token, "="); strings.HasPrefix(token, "-") && j >= 0 {
			name, inlineValue = token[:j], true
		}
		switch strings.TrimLeft(name, "-") {
		case "profile", "flow", "timeout":
			flagArgs = append(flagArgs, token)
			if !inlineValue {
				if i+1 >= len(args) {
					fmt.Fprintln(stderr, "oswald: mcp auth requires exactly one server name")
					return exitUsage
				}
				i++
				flagArgs = append(flagArgs, args[i])
			}
		case "no-browser", "n":
			flagArgs = append(flagArgs, token)
		default:
			if strings.HasPrefix(token, "-") {
				flagArgs = append(flagArgs, token)
			} else {
				rest = append(rest, token)
			}
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		fmt.Fprintln(stderr, "oswald: mcp auth requires exactly one server name")
		return exitUsage
	}
	serverName := strings.ToLower(strings.TrimSpace(rest[0]))
	abs, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintln(stderr, "oswald: invalid configuration root")
		return exitError
	}
	cfg, err := config.LoadProfiles(abs)
	if err != nil {
		fmt.Fprintln(stderr, "oswald: "+config.SafeErrorText(err))
		return exitError
	}
	profile := cfg
	profileRoot := cfg.ProfileRoot
	if *profileName != "default" {
		selected, ok := cfg.Profiles[*profileName]
		if !ok {
			fmt.Fprintf(stderr, "oswald: unknown profile %q\n", *profileName)
			return exitError
		}
		profile, profileRoot = selected, selected.ProfileRoot
	}
	var server *config.MCPServer
	for i := range profile.MCPServers {
		if profile.MCPServers[i].Name == serverName {
			server = &profile.MCPServers[i]
			break
		}
	}
	if server == nil {
		fmt.Fprintf(stderr, "oswald: unknown MCP server %q\n", serverName)
		return exitError
	}
	if strings.TrimSpace(server.Auth) != "oauth" {
		fmt.Fprintf(stderr, "oswald: MCP server %q is not configured for OAuth\n", serverName)
		return exitError
	}
	timeout := server.OAuth.Timeout
	if *timeoutRaw != "" {
		parsed, err := time.ParseDuration(*timeoutRaw)
		if err != nil || parsed <= 0 {
			fmt.Fprintln(stderr, "oswald: invalid --timeout value")
			return exitUsage
		}
		timeout = parsed
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if err := mcp.AuthorizeServer(ctx, mcp.AuthorizeOptions{
		ProfileRoot: profileRoot,
		Server:      *server,
		Flow:        *flow,
		NoBrowser:   *noBrowser,
		Timeout:     timeout,
		Stdin:       stdin,
		Stdout:      stdout,
	}); err != nil {
		if ctx.Err() != nil {
			return exitError
		}
		fmt.Fprintln(stderr, "oswald: "+config.SafeErrorText(err))
		return exitError
	}
	return exitOK
}
