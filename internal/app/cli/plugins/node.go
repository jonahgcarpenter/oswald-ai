package plugins

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/plugins"
)

// installDepsCmd installs a discovered plugin's pinned npm deps into the
// shared plugin home. The explicit invocation is the consent gate and
// requires default scope; versions are exact pins so repeats resolve
// identically. A manifest declaration wins; otherwise the recorded
// install-time metadata carries (flags or lockfile fallback).
func installDepsCmd(ctx context.Context, root, flagProfile, raw string, stdout, stderr io.Writer) int {
	name := strings.ToLower(strings.TrimSpace(raw))
	if !plugins.ValidPluginKey(name) {
		fmt.Fprintf(stderr, "oswald: invalid plugin name %q\n", raw)
		return exitUsage
	}
	if code := requireGlobalScope(flagProfile, "usage: oswald plugins install-deps <name>", stderr); code != exitOK {
		return code
	}
	globalRoot := mustAbs(root)
	winners := discoverForList(globalRoot)
	manifest, ok := winners[name]
	if !ok {
		fmt.Fprintf(stderr, "oswald: unknown plugin %q\n", raw)
		return exitError
	}
	meta, _ := plugins.ReadInstallMetadata(globalRoot, name)
	spec, ok := plugins.ResolveNodeSpec(manifest, meta)
	if !ok {
		fmt.Fprintf(stderr, "oswald: plugin %q declares no node dependencies\n", name)
		return exitError
	}
	if spec.Lockfile {
		if err := plugins.InstallNodeLockfile(ctx, globalRoot, name, "", nil); err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
		fmt.Fprintf(stdout, "installed %s node dependencies\n", name)
		return exitOK
	}
	withMeta := plugins.Manifest{Name: manifest.Name, NodePackage: spec.Package, NodeVersion: spec.Version}
	if err := plugins.InstallNodeDeps(ctx, globalRoot, withMeta, nil); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	bin, err := plugins.NodeBinPath(globalRoot, name, plugins.EffectiveNodeBin(manifest, meta, ""))
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "installed %s node dependencies\n%s\n", name, bin)
	return exitOK
}

// binPathCmd prints the absolute plugin-local binary path for explicit use in
// terminal commands. It never mutates PATH; the caller passes the absolute
// path to terminal/process_manage. Installs are shared, so this is global.
func binPathCmd(root, flagProfile string, rest []string, stdout, stderr io.Writer) int {
	bin := ""
	var names []string
	for _, token := range rest {
		if strings.HasPrefix(token, "--bin=") {
			bin = strings.TrimSpace(strings.TrimPrefix(token, "--bin="))
			continue
		}
		if token == "--bin" {
			fmt.Fprintln(stderr, "usage: oswald plugins bin-path <name> [--bin BIN]")
			return exitUsage
		}
		names = append(names, token)
	}
	if len(names) != 1 {
		fmt.Fprintln(stderr, "usage: oswald plugins bin-path <name> [--bin BIN]")
		return exitUsage
	}
	name := strings.ToLower(strings.TrimSpace(names[0]))
	if !plugins.ValidPluginKey(name) {
		fmt.Fprintf(stderr, "oswald: invalid plugin name %q\n", names[0])
		return exitUsage
	}
	if code := requireGlobalScope(flagProfile, "usage: oswald plugins bin-path <name> [--bin BIN]", stderr); code != exitOK {
		return code
	}
	globalRoot := mustAbs(root)
	winners := discoverForList(globalRoot)
	manifest, ok := winners[name]
	if !ok {
		fmt.Fprintf(stderr, "oswald: unknown plugin %q\n", names[0])
		return exitError
	}
	meta, _ := plugins.ReadInstallMetadata(globalRoot, name)
	if bin == "" {
		bin = plugins.EffectiveNodeBin(manifest, meta, "")
	}
	resolved, err := plugins.NodeBinPath(globalRoot, name, bin)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprintln(stdout, resolved)
	return exitOK
}

// requireGlobalScope enforces default scope and rejects explicit --profile
// flags: shared installs are global operations. It returns the CLI exit code
// to use (exitOK means proceed).
func requireGlobalScope(flagProfile, usage string, stderr io.Writer) int {
	if strings.TrimSpace(flagProfile) != "" && strings.ToLower(strings.TrimSpace(flagProfile)) != "default" {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	if err := config.RequireDefaultScope(); err != nil {
		reportPluginsError(stderr, err)
		return exitError
	}
	return exitOK
}
