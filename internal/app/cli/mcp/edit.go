package mcp

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// edit merges flag-provided fields over the existing mcp_servers.<name>
// entry. Unspecified fields keep their values; switching --url/--command
// clears the other transport's keys, and --no-oauth drops the OAuth block.
// OAuth block internals beyond the --oauth-* flags stay a direct config edit.
func edit(ctx context.Context, root string, args []string, stdout, stderr io.Writer) int {
	_ = ctx
	fs := flag.NewFlagSet("oswald mcp edit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := profileFlag(fs)
	var f addFlags
	f.env = keyValueFlag{}
	f.headers = keyValueFlag{}
	registerServerFlags(fs, &f)
	var noHeader, noEnv stringListFlag
	fs.Var(&noHeader, "no-header", "delete an HTTP header (repeatable)")
	fs.Var(&noEnv, "no-env", "delete a stdio environment variable (repeatable)")
	noOAuth := fs.Bool("no-oauth", false, "drop the OAuth block (auth mode kept)")
	flagArgs, rest := partitionArgs(args, withRemovalFlags(serverValueFlags()), serverBoolFlags())
	if err := fs.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		fmt.Fprintln(stderr, "usage: oswald mcp edit <server> [--profile NAME] [field flags]")
		return exitUsage
	}
	if f.enable && f.disable {
		fmt.Fprintln(stderr, "oswald: --enable and --disable are mutually exclusive")
		return exitUsage
	}
	set := setFlags(fs)
	name := serverName(rest[0])
	spec, err := buildAddSpec(&f, set, false)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitUsage
	}
	if len(noHeader) > 0 {
		spec.HeadersRemove = append([]string(nil), noHeader...)
	}
	if len(noEnv) > 0 {
		spec.EnvRemove = append([]string(nil), noEnv...)
	}
	spec.ClearOAuth = *noOAuth
	profileName, err := config.ResolveProfile(*profile)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return exitUsage
	}
	if err := config.UpdateMCPServer(root, profileName, name, spec); err != nil {
		reportMCPError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "updated MCP server %q\n", name)
	return exitOK
}

func withRemovalFlags(valueFlags map[string]bool) map[string]bool {
	out := make(map[string]bool, len(valueFlags)+2)
	for key, value := range valueFlags {
		out[key] = value
	}
	out["no-header"] = true
	out["no-env"] = true
	return out
}
