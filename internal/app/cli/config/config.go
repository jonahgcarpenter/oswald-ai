// Package config implements the `oswald config` command group: explicit,
// operator-driven reads and writes of every config.yaml section. Every write
// validates the candidate through config.Validate before anything touches
// disk, keeps the previous bytes as the undo target, and replaces the file
// atomically. Secrets are redacted in all output.
package config

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"gopkg.in/yaml.v3"
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
	profile := fs.String("profile", "default", "operator profile owning the configuration")
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
	target, err := resolveTarget(root, *profile)
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
		if len(rest) != 2 || strings.TrimSpace(rest[1]) == "" {
			fmt.Fprintln(stderr, "usage: oswald config get <path> [--profile NAME]")
			return exitUsage
		}
		return get(target, rest[1], stdout, stderr)
	case "set":
		if len(rest) != 3 || strings.TrimSpace(rest[1]) == "" {
			fmt.Fprintln(stderr, "usage: oswald config set <path> <value> [--profile NAME]")
			return exitUsage
		}
		return set(target, rest[1], rest[2], stdout, stderr)
	case "unset":
		if len(rest) != 2 || strings.TrimSpace(rest[1]) == "" {
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

// target locates the config.yaml owned by one profile.
type target struct {
	root    string
	profile string
	path    string
}

// resolveTarget validates the profile and returns its config.yaml path. The
// default profile owns the root config.yaml; named profiles own their subtree,
// which may only carry providers, model, tools, mcp_servers, and api keys.
func resolveTarget(root, profile string) (*target, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("invalid configuration root")
	}
	if profile == "" || profile == "default" {
		return &target{root: abs, profile: "default", path: filepath.Join(abs, "config.yaml")}, nil
	}
	if !config.ValidProfileName(profile) {
		return nil, fmt.Errorf("invalid profile name %q", profile)
	}
	dir := filepath.Join(abs, "profiles", profile)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("unknown profile %q", profile)
	}
	return &target{root: abs, profile: profile, path: filepath.Join(dir, "config.yaml")}, nil
}

// readDoc reads a config.yaml without following symlinks. A missing named
// profile file reads as an empty document; a missing default file is an error.
func readDoc(path, profile string) ([]byte, *yaml.Node, error) {
	data, err := readBounded(path)
	if err != nil {
		if os.IsNotExist(err) && profile != "default" {
			return []byte("{}\n"), &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
		}
		return nil, nil, err
	}
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("configuration file is not valid YAML")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("configuration file is not valid YAML")
	}
	return data, doc.Content[0], nil
}

// readBounded reads a regular non-symlink file bounded to the runtime limit.
func readBounded(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("configuration file unavailable")
	}
	if info.Size() > config.MaxConfigBytes {
		return nil, fmt.Errorf("configuration file exceeds size limit")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("configuration file unavailable")
	}
	if len(data) > config.MaxConfigBytes {
		return nil, fmt.Errorf("configuration file exceeds size limit")
	}
	return data, nil
}

func show(t *target, path string, stdout, stderr io.Writer) int {
	_, node, err := readDoc(t.path, t.profile)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	if path != "" {
		node, err = navigate(node, splitPath(path))
		if err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
	}
	fmt.Fprint(stdout, encodeRedacted(node, splitPath(path)))
	return exitOK
}

func get(t *target, path string, stdout, stderr io.Writer) int {
	_, node, err := readDoc(t.path, t.profile)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	node, err = navigate(node, splitPath(path))
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprint(stdout, encodeRedacted(node, splitPath(path)))
	return exitOK
}

func set(t *target, path, raw string, stdout, stderr io.Writer) int {
	segments := splitPath(path)
	if len(segments) == 0 {
		fmt.Fprintln(stderr, "oswald: path is required")
		return exitUsage
	}
	orig, node, err := readDoc(t.path, t.profile)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	if err := assign(node, segments, parseValue(raw)); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	candidate, err := marshal(node)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	if err := stageAndValidate(t, candidate); err != nil {
		reportConfigError(stderr, err)
		return exitError
	}
	if len(orig) > 0 {
		if err := config.SaveLastKnownGood(t.path, orig); err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
	}
	if err := config.AtomicWritePrivateFile(t.path, candidate); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "set %s\n", path)
	return exitOK
}

func unset(t *target, path string, stdout, stderr io.Writer) int {
	segments := splitPath(path)
	if len(segments) == 0 {
		fmt.Fprintln(stderr, "oswald: path is required")
		return exitUsage
	}
	orig, node, err := readDoc(t.path, t.profile)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	if err := remove(node, segments); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	candidate, err := marshal(node)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	if err := stageAndValidate(t, candidate); err != nil {
		reportConfigError(stderr, err)
		return exitError
	}
	if len(orig) > 0 {
		if err := config.SaveLastKnownGood(t.path, orig); err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
	}
	if err := config.AtomicWritePrivateFile(t.path, candidate); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "unset %s\n", path)
	return exitOK
}

func diff(t *target, stdout, stderr io.Writer) int {
	current, err := readBounded(t.path)
	if err != nil {
		if os.IsNotExist(err) && t.profile != "default" {
			current = []byte("{}\n")
		} else {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
	}
	previous, err := config.ReadLastKnownGood(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(stdout, "no changes")
			return exitOK
		}
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	currentRedacted := redactBytes(current)
	previousRedacted := redactBytes(previous)
	if bytes.Equal(currentRedacted, previousRedacted) {
		fmt.Fprintln(stdout, "no changes")
		return exitOK
	}
	fmt.Fprint(stdout, lineDiff(previousRedacted, currentRedacted))
	return exitOK
}

func undo(t *target, stdout, stderr io.Writer) int {
	previous, err := config.ReadLastKnownGood(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(stderr, "oswald: nothing to undo")
			return exitError
		}
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	if err := stageAndValidate(t, previous); err != nil {
		reportConfigError(stderr, err)
		return exitError
	}
	current, err := readBounded(t.path)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	if err := config.AtomicWritePrivateFile(t.path, previous); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	// The undone state becomes the new undo target so undo is reversible.
	if err := config.SaveLastKnownGood(t.path, current); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
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

// stageAndValidate copies the root into a temp staging directory, substitutes
// the candidate for the target file, and runs the exact startup pipeline
// against the staging copy. Live files are untouched on failure.
func stageAndValidate(t *target, candidate []byte) error {
	if len(candidate) > config.MaxConfigBytes {
		return fmt.Errorf("configuration file exceeds size limit")
	}
	staging, err := os.MkdirTemp("", "oswald-config-*")
	if err != nil {
		return fmt.Errorf("configuration file unavailable")
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := os.Chmod(staging, 0700); err != nil {
		return fmt.Errorf("configuration file unavailable")
	}
	// Global files.
	if err := stageFile(filepath.Join(t.root, "config.yaml"), filepath.Join(staging, "config.yaml"), t.path == filepath.Join(t.root, "config.yaml"), candidate); err != nil {
		return err
	}
	if err := stageOptional(filepath.Join(t.root, ".env"), filepath.Join(staging, ".env")); err != nil {
		return err
	}
	// Named profile files, including the profile under edit so the merged
	// document validates exactly as startup would load it.
	entries, _ := os.ReadDir(filepath.Join(t.root, "profiles"))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		srcDir := filepath.Join(t.root, "profiles", name)
		dstDir := filepath.Join(staging, "profiles", name)
		if err := os.MkdirAll(dstDir, 0700); err != nil {
			return fmt.Errorf("configuration file unavailable")
		}
		profileTarget := t.path == filepath.Join(srcDir, "config.yaml")
		if err := stageOptionalAs(filepath.Join(srcDir, "config.yaml"), filepath.Join(dstDir, "config.yaml"), profileTarget, candidate, []byte("{}\n")); err != nil {
			return err
		}
		if err := stageOptional(filepath.Join(srcDir, ".env"), filepath.Join(dstDir, ".env")); err != nil {
			return err
		}
	}
	// A new named profile has no directory yet; stage its candidate alone.
	if t.profile != "default" {
		dstDir := filepath.Join(staging, "profiles", t.profile)
		if _, err := os.Stat(dstDir); os.IsNotExist(err) {
			if err := os.MkdirAll(dstDir, 0700); err != nil {
				return fmt.Errorf("configuration file unavailable")
			}
			if err := os.WriteFile(filepath.Join(dstDir, "config.yaml"), candidate, 0600); err != nil {
				return fmt.Errorf("configuration file unavailable")
			}
		}
	}
	_, err = config.Validate(staging)
	return err
}

func stageFile(src, dst string, substitute bool, candidate []byte) error {
	if substitute {
		return os.WriteFile(dst, candidate, 0600)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("configuration file unavailable")
	}
	if len(data) > config.MaxConfigBytes {
		return fmt.Errorf("configuration file exceeds size limit")
	}
	return os.WriteFile(dst, data, 0600)
}

func stageOptional(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("configuration file unavailable")
	}
	if len(data) > config.MaxConfigBytes {
		return fmt.Errorf("configuration file exceeds size limit")
	}
	return os.WriteFile(dst, data, 0600)
}

func stageOptionalAs(src, dst string, substitute bool, candidate, fallback []byte) error {
	if substitute {
		return os.WriteFile(dst, candidate, 0600)
	}
	return stageOptional(src, dst)
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
