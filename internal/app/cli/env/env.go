// Package env implements the `oswald env` command group: explicit,
// operator-driven management of the private per-profile .env file. Values are
// never printed; show lists variable names only. All writes share the mediated
// core in internal/platform/config.
package env

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

// Run dispatches the env command group.
func Run(_ context.Context, root string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	_ = stdin
	fs := flag.NewFlagSet("env", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := fs.String("profile", "default", "operator profile owning the environment file")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	if len(rest) == 0 {
		envUsage(stderr)
		return exitUsage
	}
	target, err := resolveTarget(root, *profile)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	switch rest[0] {
	case "show":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "usage: oswald env show [--profile NAME]")
			return exitUsage
		}
		return show(target, stdout, stderr)
	case "set":
		if len(rest) != 3 {
			fmt.Fprintln(stderr, "usage: oswald env set <NAME> <VALUE> [--profile NAME]")
			return exitUsage
		}
		return set(target, rest[1], rest[2], stdout, stderr)
	case "unset":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "usage: oswald env unset <NAME> [--profile NAME]")
			return exitUsage
		}
		return unset(target, rest[1], stdout, stderr)
	case "help", "-h", "--help":
		envUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown env command %q\n", rest[0])
		envUsage(stderr)
		return exitUsage
	}
}

func envUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald env [--profile NAME] <show|set|unset> [options]")
	fmt.Fprintln(w, "  show             list variable names only, values never printed")
	fmt.Fprintln(w, "  set <NAME> <VALUE>   validate and atomically write one variable")
	fmt.Fprintln(w, "  unset <NAME>      remove one variable")
}

type target struct {
	root    string
	profile string
	path    string
}

func resolveTarget(root, profile string) (*target, error) {
	name := profile
	if name == "" {
		name = "default"
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("invalid configuration root")
	}
	path, err := config.EnvPathForProfile(abs, name)
	if err != nil {
		return nil, err
	}
	return &target{root: abs, profile: name, path: path}, nil
}

func show(t *target, stdout, stderr io.Writer) int {
	names, err := config.ListEnvNames(t.path)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	for _, name := range names {
		fmt.Fprintln(stdout, name)
	}
	return exitOK
}

func set(t *target, name, value string, stdout, stderr io.Writer) int {
	if err := config.SetEnvValue(t.root, t.profile, name, value); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "set %s\n", name)
	return exitOK
}

func unset(t *target, name string, stdout, stderr io.Writer) int {
	if err := config.UnsetEnvValue(t.root, t.profile, name); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "unset %s\n", name)
	return exitOK
}
