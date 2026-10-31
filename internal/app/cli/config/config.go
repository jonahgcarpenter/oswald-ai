// Package config implements the `oswald config` command group: explicit,
// operator-driven reads and writes of every config.yaml section. All
// mutation shares the mediated core in internal/platform/config (validate
// before write, last-known-good, atomic replace); secrets are redacted in
// all output.
package config

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// Exit codes mirror the parent cli convention without importing it.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// Run dispatches the config command group.
func Run(_ context.Context, root string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profileFlag := fs.String("profile", "", "operator profile owning the configuration")
	showPath := fs.String("path", "", "schema path limiting show output (config show only)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	if len(rest) == 0 {
		configUsage(stderr)
		return exitUsage
	}
	if *showPath != "" && rest[0] != "show" {
		fmt.Fprintln(stderr, "oswald: --path applies to config show only")
		return exitUsage
	}
	target, err := resolveTarget(root, *profileFlag)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	switch rest[0] {
	case "show":
		sub := flag.NewFlagSet("config show", flag.ContinueOnError)
		sub.SetOutput(stderr)
		subPath := sub.String("path", *showPath, "schema path limiting show output")
		if err := sub.Parse(rest[1:]); err != nil {
			return exitUsage
		}
		if sub.NArg() != 0 {
			fmt.Fprintln(stderr, "usage: oswald config show [--path P] [--profile NAME]")
			return exitUsage
		}
		return show(target, *subPath, stdout, stderr)
	case "get":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "usage: oswald config get <path> [--profile NAME]")
			return exitUsage
		}
		return show(target, rest[1], stdout, stderr)
	case "set":
		if len(rest) != 3 {
			fmt.Fprintln(stderr, "usage: oswald config set <path> <value> [--profile NAME]")
			return exitUsage
		}
		return set(target, rest[1], rest[2], stdout, stderr)
	case "unset":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "usage: oswald config unset <path> [--profile NAME]")
			return exitUsage
		}
		return unset(target, rest[1], stdout, stderr)
	case "diff":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "usage: oswald config diff [--profile NAME]")
			return exitUsage
		}
		return diff(target, stdout, stderr)
	case "undo":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "usage: oswald config undo [--profile NAME]")
			return exitUsage
		}
		return undo(target, stdout, stderr)
	case "reload":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "usage: oswald config reload [--profile NAME]")
			return exitUsage
		}
		return reload(target, stdout, stderr)
	case "help", "-h", "--help":
		configUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown config command %q\n", rest[0])
		configUsage(stderr)
		return exitUsage
	}
}

func configUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald config [--profile NAME] <show|get|set|unset|diff|undo|reload> [options]")
	fmt.Fprintln(w, "  show [--path P]      effective configuration, secrets redacted")
	fmt.Fprintln(w, "  get <path>           print one schema path, secrets redacted")
	fmt.Fprintln(w, "  set <path> <value>   validate and atomically write one leaf path")
	fmt.Fprintln(w, "  unset <path>         clear one optional leaf path")
	fmt.Fprintln(w, "  diff                 current vs last-known-good, redacted")
	fmt.Fprintln(w, "  undo                 restore last-known-good")
	fmt.Fprintln(w, "  reload               validate current files")
}

// target locates the configuration root and the config.yaml owned by one profile.
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

func set(t *target, path, raw string, stdout, stderr io.Writer) int {
	sub, err := config.SplitConfigPath(path)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitUsage
	}
	if err := config.SetConfigValue(t.root, t.profile, sub, raw); err != nil {
		reportConfigError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "set %s\n", path)
	return exitOK
}

func unset(t *target, path string, stdout, stderr io.Writer) int {
	sub, err := config.SplitConfigPath(path)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitUsage
	}
	if err := config.UnsetConfigValue(t.root, t.profile, sub); err != nil {
		reportConfigError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "unset %s\n", path)
	return exitOK
}

func diff(t *target, stdout, stderr io.Writer) int {
	out, err := config.DiffConfigText(t.path)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprint(stdout, out)
	return exitOK
}

func undo(t *target, stdout, stderr io.Writer) int {
	if err := config.UndoConfigValue(t.root, t.profile); err != nil {
		reportConfigError(stderr, err)
		return exitError
	}
	fmt.Fprintln(stdout, "restored last-known-good")
	return exitOK
}

func reload(t *target, stdout, stderr io.Writer) int {
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
