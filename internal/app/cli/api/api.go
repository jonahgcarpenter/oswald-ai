// Package api implements the `oswald api` command group: explicit,
// operator-driven reads and writes of the top-level api section (the
// per-profile OpenAI gateway bearer credential). All mutation goes through
// the mediated core (staged validation before write, last-known-good,
// atomic replace). Values are never printed.
package api

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

// Run dispatches the api command group.
func Run(_ context.Context, root string, args []string, stdout, stderr io.Writer) int {
	profile, rest := splitProfile(args)
	if len(rest) == 0 {
		apiUsage(stderr)
		return exitUsage
	}
	target, err := resolveTarget(root, profile)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	switch rest[0] {
	case "show":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "usage: oswald api show [--profile NAME]")
			return exitUsage
		}
		return show(target, stdout, stderr)
	case "set-key":
		return setKey(target, rest[1:], stdout, stderr)
	case "clear-key":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "usage: oswald api clear-key [--profile NAME]")
			return exitUsage
		}
		return clearKey(target, stdout, stderr)
	case "help", "-h", "--help":
		apiUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown api command %q\n", rest[0])
		apiUsage(stderr)
		return exitUsage
	}
}

func apiUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald api [--profile NAME] <show|set-key|clear-key> [options]")
	fmt.Fprintln(w, "  show                 api section, key redacted")
	fmt.Fprintln(w, "  set-key --key REF    set api.key credential literal or ${VAR} reference")
	fmt.Fprintln(w, "  clear-key            clear api.key")
}

type target struct {
	root    string
	profile string
	path    string
}

func resolveTarget(root, profile string) (*target, error) {
	name, err := config.ResolveProfile(profile)
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
	out, err := config.ShowConfigFile(t.path, t.profile, []string{"api"})
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprint(stdout, out)
	return exitOK
}

func setKey(t *target, args []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("api set-key", flag.ContinueOnError)
	sub.SetOutput(stderr)
	key := sub.String("key", "", "credential literal or ${VAR} reference")
	flagArgs, rest := partitionAPIArgs(args)
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 0 || *key == "" {
		fmt.Fprintln(stderr, "usage: oswald api set-key --key REF [--profile NAME]")
		return exitUsage
	}
	oldCfg, _ := config.Validate(t.root)
	if err := config.SetConfigValue(t.root, t.profile, []string{"api", "key"}, *key); err != nil {
		reportAPIError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "set api.key (%s)\n", effectForChange(t.root, oldCfg))
	return exitOK
}

func clearKey(t *target, stdout, stderr io.Writer) int {
	oldCfg, _ := config.Validate(t.root)
	if err := config.UnsetConfigValue(t.root, t.profile, []string{"api", "key"}); err != nil {
		reportAPIError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "cleared api.key (%s)\n", effectForChange(t.root, oldCfg))
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

// reportAPIError prints a classified diagnostic without values.
func reportAPIError(stderr io.Writer, err error) {
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

// partitionAPIArgs splits tokens so flags are accepted in any position
// relative to the --profile flag.
func partitionAPIArgs(args []string) (flagArgs, rest []string) {
	valueFlags := map[string]bool{"profile": true, "key": true}
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
