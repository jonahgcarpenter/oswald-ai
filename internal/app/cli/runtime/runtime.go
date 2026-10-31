// Package runtime implements the `oswald runtime` command group: explicit,
// operator-driven reads and writes of the top-level runtime section. All
// mutation goes through the mediated core (staged validation before write,
// last-known-good, atomic replace). Runtime changes apply live via the
// server watcher.
package runtime

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

// validLevels is the exact whitelist for runtime.log_level.
var validLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}

// Run dispatches the runtime command group. This group is global-only: it
// owns the root config.yaml and accepts no --profile flag.
func Run(_ context.Context, root string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("runtime", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	if len(rest) == 0 {
		runtimeUsage(stderr)
		return exitUsage
	}
	switch rest[0] {
	case "show":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "usage: oswald runtime show")
			return exitUsage
		}
		return show(root, stdout, stderr)
	case "set":
		return setCmd(root, rest[1:], stdout, stderr)
	case "help", "-h", "--help":
		runtimeUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown runtime command %q\n", rest[0])
		runtimeUsage(stderr)
		return exitUsage
	}
}

func runtimeUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald runtime <show|set> [options]")
	fmt.Fprintln(w, "  show                       runtime section, secrets redacted")
	fmt.Fprintln(w, "  set --log-level LEVEL      set runtime.log_level (debug|info|warn|error)")
}

func globalPath(root string) (string, string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("invalid configuration root")
	}
	path, err := config.ConfigPathForProfile(abs, "default")
	if err != nil {
		return "", "", err
	}
	return abs, path, nil
}

func show(root string, stdout, stderr io.Writer) int {
	_, path, err := globalPath(root)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	out, err := config.ShowConfigFile(path, "default", []string{"runtime"})
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprint(stdout, out)
	return exitOK
}

func setCmd(root string, args []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("runtime set", flag.ContinueOnError)
	sub.SetOutput(stderr)
	level := sub.String("log-level", "", "log level (debug|info|warn|error)")
	if err := sub.Parse(args); err != nil {
		return exitUsage
	}
	if sub.NArg() != 0 || *level == "" {
		fmt.Fprintln(stderr, "usage: oswald runtime set --log-level debug|info|warn|error")
		return exitUsage
	}
	if !validLevels[*level] {
		fmt.Fprintf(stderr, "oswald: unknown log level %q\n", *level)
		fmt.Fprintln(stderr, "usage: oswald runtime set --log-level debug|info|warn|error")
		return exitUsage
	}
	abs, _, err := globalPath(root)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	oldCfg, _ := config.Validate(abs)
	if err := config.SetConfigValue(abs, "default", []string{"runtime", "log_level"}, *level); err != nil {
		reportRuntimeError(stderr, err)
		return exitError
	}
	effect := "restarting service to apply"
	if oldCfg != nil {
		if nextCfg, err := config.Validate(abs); err == nil {
			if config.ReloadEffect(oldCfg, nextCfg) == "live" {
				effect = "applied live"
			}
		}
	}
	fmt.Fprintf(stdout, "set runtime.log_level (%s)\n", effect)
	return exitOK
}

// reportRuntimeError prints a classified diagnostic without values.
func reportRuntimeError(stderr io.Writer, err error) {
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
