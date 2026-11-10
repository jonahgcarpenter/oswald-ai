// Package delegation implements the `oswald delegation` command group:
// explicit, operator-driven reads and writes of the top-level delegation
// section (subagent model routing). All mutation goes through the mediated
// core (staged validation before write, last-known-good, atomic replace).
package delegation

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// Exit codes mirror the parent cli convention without importing it.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// Run dispatches the delegation command group.
func Run(_ context.Context, root string, args []string, stdout, stderr io.Writer) int {
	profile, rest := splitProfile(args)
	if len(rest) == 0 {
		delegationUsage(stderr)
		return exitUsage
	}
	target, err := resolveTarget(root, profile, !(rest[0] == "show"))
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	switch rest[0] {
	case "show":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "usage: oswald delegation show [--profile NAME]")
			return exitUsage
		}
		return show(target, stdout, stderr)
	case "set":
		return setCmd(target, rest[1:], stdout, stderr)
	case "unset":
		return unsetCmd(target, rest[1:], stdout, stderr)
	case "help", "-h", "--help":
		delegationUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown delegation command %q\n", rest[0])
		delegationUsage(stderr)
		return exitUsage
	}
}

func delegationUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald delegation [--profile NAME] <show|set|unset> [options]")
	fmt.Fprintln(w, "  show                       delegation section (values only, never credentials)")
	fmt.Fprintln(w, "  set --model M              delegation.model (empty inherits the profile model)")
	fmt.Fprintln(w, "  set --provider P           delegation.provider (named provider; empty inherits)")
	fmt.Fprintln(w, "  set --base-url URL         delegation.base_url (needs provider; empty inherits)")
	fmt.Fprintln(w, "  set --max-iterations N     delegation.max_iterations")
	fmt.Fprintln(w, "  set --max-children N       delegation.max_concurrent_children")
	fmt.Fprintln(w, "  set --max-depth N          delegation.max_spawn_depth")
	fmt.Fprintln(w, "  set --orchestrator B       delegation.orchestrator_enabled (true|false)")
	fmt.Fprintln(w, "  unset model|provider|base-url   clear a delegation leaf back to inherit")
}

type target struct {
	root    string
	profile string
	path    string
}

func resolveTarget(root, profile string, mutating bool) (*target, error) {
	var name string
	var err error
	if mutating {
		name, err = config.ResolveProfile(profile)
	} else {
		name, err = config.ResolveReadProfile(profile)
	}
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("invalid configuration root")
	}
	path, err := config.ConfigPathForProfile(abs, name)
	if err != nil {
		return nil, err
	}
	return &target{root: abs, profile: name, path: path}, nil
}

func show(t *target, stdout, stderr io.Writer) int {
	out, err := config.ShowConfigFile(t.path, t.profile, []string{"delegation"})
	if err != nil {
		// An absent section is a valid state, not a config error: every
		// leaf unset means mirror the profile model.
		if strings.Contains(err.Error(), "unknown path") {
			fmt.Fprintln(stdout, "delegation: not configured (delegates mirror the profile model)")
			return exitOK
		}
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprint(stdout, out)
	return exitOK
}

func setCmd(t *target, args []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("delegation set", flag.ContinueOnError)
	sub.SetOutput(stderr)
	model := sub.String("model", "", "delegation model id")
	provider := sub.String("provider", "", "delegation provider name")
	baseURL := sub.String("base-url", "", "delegation endpoint override")
	maxIterations := sub.String("max-iterations", "", "delegation max_iterations")
	maxChildren := sub.String("max-children", "", "delegation max_concurrent_children")
	maxDepth := sub.String("max-depth", "", "delegation max_spawn_depth")
	orchestrator := sub.String("orchestrator", "", "delegation orchestrator_enabled (true|false)")
	flagArgs, rest := partitionSetArgs(args)
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 0 {
		fmt.Fprintln(stderr, "usage: oswald delegation set [--model M | --provider P | --base-url URL | --max-iterations N | --max-children N | --max-depth N | --orchestrator true|false] [--profile NAME]")
		return exitUsage
	}
	set := 0
	for _, value := range []string{*model, *provider, *baseURL, *maxIterations, *maxChildren, *maxDepth, *orchestrator} {
		if value != "" {
			set++
		}
	}
	if set == 0 {
		fmt.Fprintln(stderr, "usage: oswald delegation set [--model M | --provider P | --base-url URL | --max-iterations N | --max-children N | --max-depth N | --orchestrator true|false] [--profile NAME]")
		return exitUsage
	}
	if set > 1 {
		fmt.Fprintln(stderr, "oswald: set one delegation field per invocation")
		return exitUsage
	}
	if *maxIterations != "" {
		if _, err := boundedInt(*maxIterations, 1, 10000, "delegation.max_iterations"); err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitUsage
		}
	}
	if *maxChildren != "" {
		if _, err := boundedInt(*maxChildren, 1, 32, "delegation.max_concurrent_children"); err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitUsage
		}
	}
	if *maxDepth != "" {
		if _, err := boundedInt(*maxDepth, 1, 8, "delegation.max_spawn_depth"); err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitUsage
		}
	}
	if *orchestrator != "" {
		if *orchestrator != "true" && *orchestrator != "false" {
			fmt.Fprintln(stderr, "oswald: --orchestrator must be true or false")
			return exitUsage
		}
	}
	oldCfg, _ := config.Validate(t.root)
	var err error
	switch {
	case *model != "":
		err = config.SetConfigValue(t.root, t.profile, []string{"delegation", "model"}, *model)
	case *provider != "":
		err = config.SetConfigValue(t.root, t.profile, []string{"delegation", "provider"}, *provider)
	case *baseURL != "":
		err = config.SetConfigValue(t.root, t.profile, []string{"delegation", "base_url"}, *baseURL)
	case *maxIterations != "":
		err = config.SetConfigValue(t.root, t.profile, []string{"delegation", "max_iterations"}, *maxIterations)
	case *maxChildren != "":
		err = config.SetConfigValue(t.root, t.profile, []string{"delegation", "max_concurrent_children"}, *maxChildren)
	case *maxDepth != "":
		err = config.SetConfigValue(t.root, t.profile, []string{"delegation", "max_spawn_depth"}, *maxDepth)
	case *orchestrator != "":
		err = config.SetConfigValue(t.root, t.profile, []string{"delegation", "orchestrator_enabled"}, *orchestrator)
	}
	if err != nil {
		reportDelegationError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "set delegation (%s)\n", effectForChange(t.root, oldCfg))
	return exitOK
}

func unsetCmd(t *target, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: oswald delegation unset <model|provider|base-url> [--profile NAME]")
		return exitUsage
	}
	var leaf string
	switch args[0] {
	case "model":
		leaf = "model"
	case "provider":
		leaf = "provider"
	case "base-url", "base_url":
		leaf = "base_url"
	default:
		fmt.Fprintln(stderr, "usage: oswald delegation unset <model|provider|base-url> [--profile NAME]")
		return exitUsage
	}
	oldCfg, _ := config.Validate(t.root)
	if err := config.UnsetConfigValue(t.root, t.profile, []string{"delegation", leaf}); err != nil {
		reportDelegationError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "unset delegation.%s (%s)\n", leaf, effectForChange(t.root, oldCfg))
	return exitOK
}

func boundedInt(raw string, min, max int, leaf string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("invalid %s (want %d-%d)", leaf, min, max)
	}
	return n, nil
}

func effectForChange(root string, oldCfg *config.Config) string {
	if oldCfg != nil {
		if nextCfg, err := config.Validate(root); err == nil {
			if config.ReloadEffect(oldCfg, nextCfg) == "live" {
				return "applied live"
			}
		}
	}
	return "restarting service to apply"
}

// reportDelegationError prints a classified diagnostic without values.
func reportDelegationError(stderr io.Writer, err error) {
	if cerr, ok := config.AsConfigError(err); ok {
		location := cerr.Path
		if location == "" {
			location = cerr.Source
		}
		if location != "" {
			fmt.Fprintf(stderr, "oswald: invalid configuration: %s at %s\n", cerr.Code, location)
			return
		}
		fmt.Fprintf(stderr, "oswald: invalid configuration: %s\n", cerr.Code)
		return
	}
	fmt.Fprintf(stderr, "oswald: %v\n", err)
}

// splitProfile extracts --profile NAME / --profile=NAME in any position,
// returning the profile value and remaining args.
func splitProfile(args []string) (string, []string) {
	var rest []string
	profile := ""
	for i := 0; i < len(args); i++ {
		token := args[i]
		if token == "--profile" {
			if i+1 < len(args) {
				i++
				profile = args[i]
			}
			continue
		}
		if strings.HasPrefix(token, "--profile=") {
			profile = strings.TrimPrefix(token, "--profile=")
			continue
		}
		rest = append(rest, token)
	}
	return profile, rest
}

// partitionSetArgs splits tokens so flags are accepted in any position
// relative to the --profile flag.
func partitionSetArgs(args []string) (flagArgs, rest []string) {
	valueFlags := map[string]bool{"profile": true, "model": true, "provider": true, "base-url": true, "max-iterations": true, "max-children": true, "max-depth": true, "orchestrator": true}
	for i := 0; i < len(args); i++ {
		token := args[i]
		name := token
		if j := strings.Index(token, "="); strings.HasPrefix(token, "-") && j >= 0 {
			name = token[:j]
		}
		key := strings.TrimLeft(name, "-")
		if valueFlags[key] {
			flagArgs = append(flagArgs, token)
			if !strings.Contains(token, "=") {
				if i+1 < len(args) {
					i++
					flagArgs = append(flagArgs, args[i])
				}
			}
			continue
		}
		if strings.HasPrefix(token, "-") {
			flagArgs = append(flagArgs, token)
			continue
		}
		rest = append(rest, token)
	}
	return flagArgs, rest
}
