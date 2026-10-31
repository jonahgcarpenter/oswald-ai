package mcp

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// addFlags holds the shared server-shape flags for add and edit.
type addFlags struct {
	url           string
	command       string
	args          stringListFlag
	cwd           string
	env           keyValueFlag
	transport     string
	headers       keyValueFlag
	auth          string
	oauthClientID string
	oauthSecret   string
	oauthFlow     string
	oauthScopes   string
	oauthTimeout  string
	include       string
	exclude       string
	enable        bool
	disable       bool
}

func registerServerFlags(fs *flag.FlagSet, f *addFlags) {
	fs.StringVar(&f.url, "url", "", "HTTP endpoint URL (streamable HTTP or SSE)")
	fs.StringVar(&f.command, "command", "", "stdio command (exactly one of --url or --command)")
	fs.Var(&f.args, "arg", "stdio argument (repeatable; replaces on edit)")
	fs.StringVar(&f.cwd, "cwd", "", "stdio working directory, profile-relative")
	fs.Var(&f.env, "env", "stdio environment as KEY=VALUE (repeatable)")
	fs.StringVar(&f.transport, "transport", "", "HTTP transport: streamable_http or sse")
	fs.Var(&f.headers, "header", "HTTP header as KEY=VALUE (repeatable; static auth)")
	fs.StringVar(&f.auth, "auth", "", "auth mode: oauth or none")
	fs.StringVar(&f.oauthClientID, "oauth-client-id", "", "OAuth client ID")
	fs.StringVar(&f.oauthSecret, "oauth-client-secret", "", "OAuth client secret")
	fs.StringVar(&f.oauthFlow, "oauth-flow", "", "OAuth flow: browser or device")
	fs.StringVar(&f.oauthScopes, "oauth-scopes", "", "comma-separated OAuth scopes (replaces)")
	fs.StringVar(&f.oauthTimeout, "oauth-timeout", "", "OAuth wait bound, e.g. 5m")
	fs.StringVar(&f.include, "include", "", "comma-separated tool allowlist (replaces)")
	fs.StringVar(&f.exclude, "exclude", "", "comma-separated tool blocklist (replaces)")
	fs.BoolVar(&f.enable, "enable", false, "mark the server enabled")
	fs.BoolVar(&f.disable, "disable", false, "mark the server disabled")
}

func serverValueFlags() map[string]bool {
	return map[string]bool{
		"profile": true, "url": true, "command": true, "arg": true, "cwd": true,
		"env": true, "transport": true, "header": true, "auth": true,
		"oauth-client-id": true, "oauth-client-secret": true, "oauth-flow": true,
		"oauth-scopes": true, "oauth-timeout": true, "include": true, "exclude": true,
	}
}

func serverBoolFlags() map[string]bool {
	return map[string]bool{"enable": true, "disable": true, "with-tokens": true, "no-header": true, "no-env": true, "no-oauth": true}
}

// add inserts a new mcp_servers.<name> entry through staged validation.
// Secrets travel on argv; they are never echoed or logged, matching the
// operator visibility posture of `oswald env set`.
func add(ctx context.Context, root string, args []string, stdout, stderr io.Writer) int {
	_ = ctx
	fs := flag.NewFlagSet("oswald mcp add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := profileFlag(fs)
	var f addFlags
	f.env = keyValueFlag{}
	f.headers = keyValueFlag{}
	registerServerFlags(fs, &f)
	flagArgs, rest := partitionArgs(args, serverValueFlags(), serverBoolFlags())
	if err := fs.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		fmt.Fprintln(stderr, "usage: oswald mcp add <server> [--profile NAME] (--url URL | --command CMD) [options]")
		return exitUsage
	}
	if f.enable && f.disable {
		fmt.Fprintln(stderr, "oswald: --enable and --disable are mutually exclusive")
		return exitUsage
	}
	name := serverName(rest[0])
	spec, err := buildAddSpec(&f, setFlags(fs), true)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitUsage
	}
	profileName, err := config.ResolveProfile(*profile)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return exitUsage
	}
	if err := config.AddMCPServer(root, profileName, name, spec); err != nil {
		reportMCPError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "added MCP server %q\n", name)
	return exitOK
}

// buildAddSpec converts parsed flags into a full server spec. Every field
// the operator did not pass stays zero; validation runs on the staged write.
func buildAddSpec(f *addFlags, set map[string]bool, requireTransport bool) (config.MCPServerSpec, error) {
	var spec config.MCPServerSpec
	if set["url"] {
		spec.URL = &f.url
	}
	if set["command"] {
		spec.Command = &f.command
	}
	if set["arg"] {
		values := append([]string(nil), f.args...)
		spec.Args = &values
	}
	if set["cwd"] {
		spec.Cwd = &f.cwd
	}
	if set["env"] {
		spec.EnvSet = map[string]string(f.env)
	}
	if set["transport"] {
		spec.Transport = &f.transport
	}
	if set["header"] {
		spec.HeadersSet = map[string]string(f.headers)
	}
	if set["auth"] {
		spec.Auth = &f.auth
	}
	if set["oauth-client-id"] {
		spec.OAuthClientID = &f.oauthClientID
	}
	if set["oauth-client-secret"] {
		spec.OAuthClientSecret = &f.oauthSecret
	}
	if set["oauth-flow"] {
		spec.OAuthFlow = &f.oauthFlow
	}
	if set["oauth-scopes"] {
		scopes := splitCSV(f.oauthScopes)
		spec.OAuthScopes = &scopes
	}
	if set["oauth-timeout"] {
		spec.OAuthTimeout = &f.oauthTimeout
	}
	if set["include"] {
		include := splitCSV(f.include)
		spec.Include = &include
	}
	if set["exclude"] {
		exclude := splitCSV(f.exclude)
		spec.Exclude = &exclude
	}
	if set["enable"] {
		spec.Enabled = boolPointer(true)
	} else if set["disable"] {
		spec.Enabled = boolPointer(false)
	}
	if requireTransport && strings.TrimSpace(f.url) == "" && strings.TrimSpace(f.command) == "" {
		return config.MCPServerSpec{}, fmt.Errorf("server needs exactly one of --url or --command")
	}
	return spec, nil
}

func boolPointer(value bool) *bool { return &value }
