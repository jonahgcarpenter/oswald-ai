package plugins

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/plugins"
)

// execCmd runs one installed plugin binary for the calling profile. The child
// gets the fixed sandbox base plus that profile's passthrough allowlist, and
// HOME pointed at the per-profile plugin data home so CLI config files land
// on the persisted volume. Stdin is closed: only non-interactive flags work.
// Presence means enabled.
func execCmd(ctx context.Context, root, flagProfile string, rest []string, stdout, stderr io.Writer) int {
	var names []string
	var bin string
	timeout := 0
	var args []string
	parsingArgs := false
	for i := 0; i < len(rest); i++ {
		token := rest[i]
		if parsingArgs {
			args = append(args, token)
			continue
		}
		switch {
		case token == "--":
			parsingArgs = true
		case token == "--bin" && i+1 < len(rest):
			i++
			bin = rest[i]
		case strings.HasPrefix(token, "--bin="):
			bin = strings.TrimPrefix(token, "--bin=")
		case token == "--timeout" && i+1 < len(rest):
			i++
			timeout = parseTimeout(rest[i])
		case strings.HasPrefix(token, "--timeout="):
			timeout = parseTimeout(strings.TrimPrefix(token, "--timeout="))
		case strings.HasPrefix(token, "-"):
			fmt.Fprintln(stderr, "usage: oswald plugins exec <name> [--bin BIN] [--timeout SECS] [-- <args...>] [--profile NAME]")
			return exitUsage
		default:
			names = append(names, token)
		}
	}
	if len(names) != 1 {
		fmt.Fprintln(stderr, "usage: oswald plugins exec <name> [--bin BIN] [--timeout SECS] [-- <args...>] [--profile NAME]")
		return exitUsage
	}
	key := strings.ToLower(strings.TrimSpace(names[0]))
	if !plugins.ValidPluginKey(key) {
		fmt.Fprintf(stderr, "oswald: invalid plugin name %q\n", names[0])
		return exitUsage
	}
	_, profileName, profileRoot, err := resolveScope(root, flagProfile, true)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	winners := discoverForList(profileRoot)
	manifest, ok := winners[key]
	if !ok {
		fmt.Fprintf(stderr, "oswald: plugin %q is not installed for profile %q\n", names[0], profileName)
		return exitError
	}
	meta, _ := plugins.ReadInstallMetadata(profileRoot, key)
	if bin == "" {
		bin = plugins.EffectiveNodeBin(manifest, meta, "")
	}
	output, err := plugins.ExecPluginBinary(ctx, profileRoot, key, bin, args, timeout, nil)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprintln(stdout, output)
	return exitOK
}

func parseTimeout(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
