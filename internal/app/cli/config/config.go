// Package config implements the `oswald config` command group: read-only
// inspection plus staged validation of config.yaml. Mutation lives in the
// scoped groups (providers, model, api, gateway, platforms, tools, runtime,
// mcp); this group never writes. Secrets are redacted in all output.
package config

import (
	"context"
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

// Run dispatches the config command group. --profile and --path are accepted
// in any position.
func Run(_ context.Context, root string, args []string, stdout, stderr io.Writer) int {
	profile, showPath, rest := splitConfigArgs(args)
	if len(rest) == 0 {
		configUsage(stderr)
		return exitUsage
	}
	if showPath != "" && rest[0] != "show" {
		fmt.Fprintln(stderr, "oswald: --path applies to config show only")
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
			fmt.Fprintln(stderr, "usage: oswald config show [--path P] [--profile NAME]")
			return exitUsage
		}
		return show(target, showPath, stdout, stderr)
	case "get":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "usage: oswald config get <path> [--profile NAME]")
			return exitUsage
		}
		return show(target, rest[1], stdout, stderr)
	case "validate":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "usage: oswald config validate [--profile NAME]")
			return exitUsage
		}
		return validate(target, stdout, stderr)
	case "help", "-h", "--help":
		configUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown config command %q\n", rest[0])
		configUsage(stderr)
		return exitUsage
	}
}

// splitConfigArgs extracts --profile and --path in any position, returning
// their values and the remaining args.
func splitConfigArgs(args []string) (profile, showPath string, rest []string) {
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
		if token == "--path" {
			if i+1 < len(args) {
				i++
				showPath = args[i]
			}
			continue
		}
		if strings.HasPrefix(token, "--path=") {
			showPath = strings.TrimPrefix(token, "--path=")
			continue
		}
		rest = append(rest, token)
	}
	return profile, showPath, rest
}

func configUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald config [--profile NAME] <show|get|validate> [options]")
	fmt.Fprintln(w, "  show [--path P]      effective configuration, secrets redacted")
	fmt.Fprintln(w, "  get <path>           print one schema path, secrets redacted")
	fmt.Fprintln(w, "  validate             validate current files")
	fmt.Fprintln(w, "  (mutation lives in the scoped groups: providers, model, api,")
	fmt.Fprintln(w, "   gateway, platforms, tools, runtime, mcp)")
}

// target locates the configuration root and the config.yaml owned by one profile.
type target struct {
	root    string
	profile string
	path    string
}

func resolveTarget(root, profile string) (*target, error) {
	name, err := config.ResolveReadProfile(profile)
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

func show(t *target, path string, stdout, stderr io.Writer) int {
	var sub []string
	if path != "" {
		var err error
		sub, err = config.SplitConfigPath(path)
		if err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitUsage
		}
	}
	out, err := config.ShowConfigFile(t.path, t.profile, sub)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprint(stdout, out)
	return exitOK
}

func validate(t *target, stdout, stderr io.Writer) int {
	if _, err := config.Validate(t.root); err != nil {
		reportConfigError(stderr, err)
		return exitError
	}
	fmt.Fprintln(stdout, "configuration valid")
	return exitOK
}

// reportConfigError prints a classified diagnostic without values.
func reportConfigError(stderr io.Writer, err error) {
	if cerr, ok := config.AsConfigError(err); ok {
		location := cerr.Path
		if location == "" {
			location = cerr.Source
		}
		if variable := cerr.Variable; variable != "" && location != "" {
			fmt.Fprintf(stderr, "oswald: invalid configuration: %s at %s (variable %s)\n", cerr.Code, location, variable)
			return
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
