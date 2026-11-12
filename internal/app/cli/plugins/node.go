package plugins

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/plugins"
)

// installDepsCmd installs a profile plugin's pinned npm deps into the
// profile code home. The explicit invocation is the consent gate;
// versions are exact pins so repeats resolve identically. A manifest
// declaration wins; otherwise the recorded install-time metadata carries
// (flags or lockfile fallback).
func installDepsCmd(ctx context.Context, root, flagProfile, raw string, stdout, stderr io.Writer) int {
	name := strings.ToLower(strings.TrimSpace(raw))
	if !plugins.ValidPluginKey(name) {
		fmt.Fprintf(stderr, "oswald: invalid plugin name %q\n", raw)
		return exitUsage
	}
	_, _, profileRoot, err := resolveScope(root, flagProfile, true)
	if err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	winners := discoverForList(profileRoot)
	manifest, ok := winners[name]
	if !ok {
		fmt.Fprintf(stderr, "oswald: unknown plugin %q\n", raw)
		return exitError
	}
	meta, _ := plugins.ReadInstallMetadata(profileRoot, name)
	spec, ok := plugins.ResolveNodeSpec(manifest, meta)
	if !ok {
		fmt.Fprintf(stderr, "oswald: plugin %q declares no node dependencies\n", name)
		return exitError
	}
	if spec.Lockfile {
		if err := plugins.InstallNodeLockfile(ctx, profileRoot, name, "", nil); err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
		fmt.Fprintf(stdout, "installed %s node dependencies (restart required - run `oswald restart` to apply)\n", name)
		return exitOK
	}
	withMeta := plugins.Manifest{Name: manifest.Name, NodePackage: spec.Package, NodeVersion: spec.Version}
	if err := plugins.InstallNodeDeps(ctx, profileRoot, withMeta, nil); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	// Shims regenerate inside the install call, so the bare name is
	// runnable in terminal immediately; tool/hook registration still
	// needs `oswald restart`.
	if bin := plugins.EffectiveNodeBin(manifest, meta, ""); bin != "" {
		fmt.Fprintf(stdout, "installed %s node dependencies ('%s' runnable now; restart required - run `oswald restart` to apply)\n", name, bin)
		return exitOK
	}
	fmt.Fprintf(stdout, "installed %s node dependencies (restart required - run `oswald restart` to apply)\n", name)
	return exitOK
}
