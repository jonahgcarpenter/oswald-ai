// Package env implements the `oswald env` command group: explicit,
// operator-driven management of the private per-profile .env file. Values are
// never printed; show lists variable names only. Every write validates the
// candidate through config.Validate before anything touches disk.
package env

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/joho/godotenv"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// Exit codes mirror the parent cli convention without importing it.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
		if len(rest) != 3 || !envNameRE.MatchString(rest[1]) {
			fmt.Fprintln(stderr, "usage: oswald env set <NAME> <VALUE> [--profile NAME]")
			return exitUsage
		}
		return set(target, rest[1], rest[2], stdout, stderr)
	case "unset":
		if len(rest) != 2 || !envNameRE.MatchString(rest[1]) {
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
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("invalid configuration root")
	}
	if profile == "" || profile == "default" {
		return &target{root: abs, profile: "default", path: filepath.Join(abs, ".env")}, nil
	}
	if !config.ValidProfileName(profile) {
		return nil, fmt.Errorf("invalid profile name %q", profile)
	}
	dir := filepath.Join(abs, "profiles", profile)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("unknown profile %q", profile)
	}
	return &target{root: abs, profile: profile, path: filepath.Join(dir, ".env")}, nil
}

func show(t *target, stdout, stderr io.Writer) int {
	values, err := readEnv(t.path)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintln(stdout, name)
	}
	return exitOK
}

func set(t *target, name, value string, stdout, stderr io.Writer) int {
	orig, values, err := readEnvRaw(t.path)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	values[name] = value
	candidate, err := godotenv.Marshal(values)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	if !strings.HasSuffix(candidate, "\n") {
		candidate += "\n"
	}
	if err := stageAndValidateEnv(t, []byte(candidate)); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	if orig != nil {
		if err := config.SaveLastKnownGood(t.path, orig); err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
	}
	if err := config.AtomicWritePrivateFile(t.path, []byte(candidate)); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "set %s\n", name)
	return exitOK
}

func unset(t *target, name string, stdout, stderr io.Writer) int {
	orig, values, err := readEnvRaw(t.path)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	if _, ok := values[name]; !ok {
		fmt.Fprintf(stderr, "oswald: unknown variable %q\n", name)
		return exitError
	}
	delete(values, name)
	candidate, err := godotenv.Marshal(values)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	if candidate != "" && !strings.HasSuffix(candidate, "\n") {
		candidate += "\n"
	}
	if err := stageAndValidateEnv(t, []byte(candidate)); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	if orig != nil {
		if err := config.SaveLastKnownGood(t.path, orig); err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
	}
	if err := config.AtomicWritePrivateFile(t.path, []byte(candidate)); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "unset %s\n", name)
	return exitOK
}

// readEnv parses the .env file; a missing file reads as empty.
func readEnv(path string) (map[string]string, error) {
	_, values, err := readEnvRaw(path)
	return values, err
}

func readEnvRaw(path string) ([]byte, map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, map[string]string{}, nil
		}
		return nil, nil, fmt.Errorf("environment file unavailable")
	}
	if len(data) > config.MaxConfigBytes {
		return nil, nil, fmt.Errorf("environment file exceeds size limit")
	}
	if len(data) == 0 {
		return data, map[string]string{}, nil
	}
	values, err := godotenv.Unmarshal(string(data))
	if err != nil {
		return nil, nil, fmt.Errorf("environment file is not valid")
	}
	return data, values, nil
}

// stageAndValidateEnv stages the candidate .env into a temp root copy and
// validates the exact startup pipeline against it.
func stageAndValidateEnv(t *target, candidate []byte) error {
	if len(candidate) > config.MaxConfigBytes {
		return fmt.Errorf("environment file exceeds size limit")
	}
	if _, err := godotenv.Unmarshal(string(candidate)); len(candidate) > 0 && err != nil {
		return fmt.Errorf("environment file is not valid")
	}
	staging, err := os.MkdirTemp("", "oswald-env-*")
	if err != nil {
		return fmt.Errorf("environment file unavailable")
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := os.Chmod(staging, 0700); err != nil {
		return fmt.Errorf("environment file unavailable")
	}
	globalEnv := filepath.Join(t.root, ".env")
	stageCandidate := t.path == globalEnv
	if err := stageCopy(filepath.Join(t.root, "config.yaml"), filepath.Join(staging, "config.yaml")); err != nil {
		return err
	}
	if stageCandidate {
		if err := os.WriteFile(filepath.Join(staging, ".env"), candidate, 0600); err != nil {
			return fmt.Errorf("environment file unavailable")
		}
	} else if err := stageCopy(globalEnv, filepath.Join(staging, ".env")); err != nil {
		return err
	}
	entries, _ := os.ReadDir(filepath.Join(t.root, "profiles"))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		srcDir := filepath.Join(t.root, "profiles", entry.Name())
		dstDir := filepath.Join(staging, "profiles", entry.Name())
		if err := os.MkdirAll(dstDir, 0700); err != nil {
			return fmt.Errorf("environment file unavailable")
		}
		if err := stageCopy(filepath.Join(srcDir, "config.yaml"), filepath.Join(dstDir, "config.yaml")); err != nil {
			return err
		}
		if t.path == filepath.Join(srcDir, ".env") {
			if err := os.WriteFile(filepath.Join(dstDir, ".env"), candidate, 0600); err != nil {
				return fmt.Errorf("environment file unavailable")
			}
		} else if err := stageCopyOptional(filepath.Join(srcDir, ".env"), filepath.Join(dstDir, ".env")); err != nil {
			return err
		}
	}
	if _, err := config.Validate(staging); err != nil {
		if cerr, ok := config.AsConfigError(err); ok {
			location := cerr.Path
			if location == "" {
				location = cerr.Source
			}
			if location != "" {
				return fmt.Errorf("invalid configuration: %s at %s", cerr.Code, location)
			}
			return fmt.Errorf("invalid configuration: %s", cerr.Code)
		}
		return err
	}
	return nil
}

func stageCopy(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("configuration file unavailable")
	}
	if len(data) > config.MaxConfigBytes {
		return fmt.Errorf("configuration file exceeds size limit")
	}
	return os.WriteFile(dst, data, 0600)
}

func stageCopyOptional(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("environment file unavailable")
	}
	if len(data) > config.MaxConfigBytes {
		return fmt.Errorf("environment file exceeds size limit")
	}
	return os.WriteFile(dst, data, 0600)
}
