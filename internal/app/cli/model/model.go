// Package model implements the `oswald model` command group: explicit,
// operator-driven reads and writes of the top-level model section. All
// mutation goes through the mediated core (staged validation before write,
// last-known-good, atomic replace).
package model

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// Exit codes mirror the parent cli convention without importing it.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// Run dispatches the model command group.
func Run(_ context.Context, root string, args []string, stdout, stderr io.Writer) int {
	profile, rest := splitProfile(args)
	if len(rest) == 0 {
		modelUsage(stderr)
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
			fmt.Fprintln(stderr, "usage: oswald model show [--profile NAME]")
			return exitUsage
		}
		return show(target, stdout, stderr)
	case "set":
		return setCmd(target, rest[1:], stdout, stderr)
	case "help", "-h", "--help":
		modelUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown model command %q\n", rest[0])
		modelUsage(stderr)
		return exitUsage
	}
}

func modelUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald model [--profile NAME] <show|set> [options]")
	fmt.Fprintln(w, "  show                       model section")
	fmt.Fprintln(w, "  set --provider P           set model.provider (custom:<name>)")
	fmt.Fprintln(w, "  set --default M            set model.default")
	fmt.Fprintln(w, "  set --context-length N     set model.context_length")
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
	out, err := config.ShowConfigFile(t.path, t.profile, []string{"model"})
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprint(stdout, out)
	return exitOK
}

func setCmd(t *target, args []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("model set", flag.ContinueOnError)
	sub.SetOutput(stderr)
	provider := sub.String("provider", "", "model provider (custom:<name>)")
	def := sub.String("default", "", "default model id")
	contextLength := sub.String("context-length", "", "context window size")
	flagArgs, rest := partitionSetArgs(args)
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 0 {
		fmt.Fprintln(stderr, "usage: oswald model set [--provider P] [--default M] [--context-length N] [--profile NAME]")
		return exitUsage
	}
	set := 0
	if *provider != "" {
		set++
	}
	if *def != "" {
		set++
	}
	if *contextLength != "" {
		set++
	}
	if set == 0 {
		fmt.Fprintln(stderr, "usage: oswald model set [--provider P] [--default M] [--context-length N] [--profile NAME]")
		return exitUsage
	}
	if set > 1 {
		fmt.Fprintln(stderr, "oswald: set one model field per invocation")
		return exitUsage
	}
	oldCfg, _ := config.Validate(t.root)
	var err error
	switch {
	case *provider != "":
		err = config.SetConfigValue(t.root, t.profile, []string{"model", "provider"}, *provider)
	case *def != "":
		err = config.SetConfigValue(t.root, t.profile, []string{"model", "default"}, *def)
	case *contextLength != "":
		err = config.SetConfigValue(t.root, t.profile, []string{"model", "context_length"}, *contextLength)
	}
	if err != nil {
		reportModelError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "set model (%s)\n", effectForChange(t.root, oldCfg))
	return exitOK
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

// reportModelError prints a classified diagnostic without values.
func reportModelError(stderr io.Writer, err error) {
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

// partitionSetArgs splits tokens so flags are accepted in any position
// relative to the --profile flag.
func partitionSetArgs(args []string) (flagArgs, rest []string) {
	valueFlags := map[string]bool{"profile": true, "provider": true, "default": true, "context-length": true}
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
