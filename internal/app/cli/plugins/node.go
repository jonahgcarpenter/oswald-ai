package plugins

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/plugins"
)

// installDepsCmd installs a discovered plugin's pinned npm deps into the
// calling profile's plugin home. The explicit invocation is the consent gate;
// versions are exact pins so repeats resolve identically.
func installDepsCmd(ctx context.Context, root, flagProfile, raw string, stdout, stderr io.Writer) int {
	name := strings.ToLower(strings.TrimSpace(raw))
	if !plugins.ValidPluginKey(name) {
		fmt.Fprintf(stderr, "oswald: invalid plugin name %q\n", raw)
		return exitUsage
	}
	_, profileName, profileRoot, err := resolveScope(root, flagProfile, true)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	winners := discoverForList(globalRootFor(profileRoot, profileName))
	key := name
	manifest, ok := winners[key]
	if !ok {
		fmt.Fprintf(stderr, "oswald: unknown plugin %q\n", raw)
		return exitError
	}
	if strings.TrimSpace(manifest.NodePackage) == "" {
		fmt.Fprintf(stderr, "oswald: plugin %q declares no node dependencies\n", key)
		return exitError
	}
	if err := plugins.InstallNodeDeps(ctx, profileRoot, manifest, nil); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	bin, err := plugins.NodeBinPath(profileRoot, key, plugins.DefaultNodeBin(manifest))
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "installed %s node dependencies\n%s\n", key, bin)
	return exitOK
}

// binPathCmd prints the absolute plugin-local binary path for explicit use in
// terminal commands. It never mutates PATH; the caller passes the absolute
// path to terminal/process_manage.
func binPathCmd(root, flagProfile string, rest []string, stdout, stderr io.Writer) int {
	bin := ""
	var names []string
	for _, token := range rest {
		if strings.HasPrefix(token, "--bin=") {
			bin = strings.TrimSpace(strings.TrimPrefix(token, "--bin="))
			continue
		}
		if token == "--bin" {
			fmt.Fprintln(stderr, "usage: oswald plugins bin-path <name> [--bin BIN] [--profile NAME]")
			return exitUsage
		}
		names = append(names, token)
	}
	if len(names) != 1 {
		fmt.Fprintln(stderr, "usage: oswald plugins bin-path <name> [--bin BIN] [--profile NAME]")
		return exitUsage
	}
	name := strings.ToLower(strings.TrimSpace(names[0]))
	if !plugins.ValidPluginKey(name) {
		fmt.Fprintf(stderr, "oswald: invalid plugin name %q\n", names[0])
		return exitUsage
	}
	_, profileName, profileRoot, err := resolveScope(root, flagProfile, false)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	winners := discoverForList(globalRootFor(profileRoot, profileName))
	key := name
	manifest, ok := winners[key]
	if !ok {
		fmt.Fprintf(stderr, "oswald: unknown plugin %q\n", names[0])
		return exitError
	}
	if bin == "" {
		bin = plugins.DefaultNodeBin(manifest)
	}
	resolved, err := plugins.NodeBinPath(profileRoot, key, bin)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprintln(stdout, resolved)
	return exitOK
}
