// Package platforms implements the `oswald platforms` command group:
// explicit, operator-driven management of the top-level platforms section
// (gateway listeners). It owns platforms.<name>.enabled plus the extra
// listener/admission leaves. All mutation goes through the mediated core
// (staged validation before write, last-known-good, atomic replace).
// This group is global-only.
package platforms

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"gopkg.in/yaml.v3"
)

// Exit codes mirror the parent cli convention without importing it.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// maxConfigBytes bounds the configuration file, matching the runtime read limit.
const maxConfigBytes = 256 * 1024

// validPlatforms is the exact set of platforms.<name> keys this command manages.
var validPlatforms = map[string]bool{"discord": true, "bluebubbles": true, "api": true}

// Run dispatches the platforms command group. Global-only: no --profile flag.
func Run(_ context.Context, root string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		platformsUsage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "show":
		if len(args) > 2 {
			fmt.Fprintln(stderr, "usage: oswald platforms show [name]")
			return exitUsage
		}
		name := ""
		if len(args) == 2 {
			name = args[1]
		}
		return show(root, name, stdout, stderr)
	case "enable", "disable", "set":
		if err := config.RequireDefaultScope(); err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return exitError
		}
		switch args[0] {
		case "enable":
			if len(args) != 2 {
				fmt.Fprintln(stderr, "usage: oswald platforms enable <discord|bluebubbles|api>")
				return exitUsage
			}
			return setEnabledCmd(root, args[1], true, stdout, stderr)
		case "disable":
			if len(args) != 2 {
				fmt.Fprintln(stderr, "usage: oswald platforms disable <discord|bluebubbles|api>")
				return exitUsage
			}
			return setEnabledCmd(root, args[1], false, stdout, stderr)
		default:
			return setCmd(root, args[1:], stdout, stderr)
		}
	case "help", "-h", "--help":
		platformsUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown platforms command %q\n", args[0])
		platformsUsage(stderr)
		return exitUsage
	}
}

func platformsUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald platforms <show|enable|disable|set> [options]")
	fmt.Fprintln(w, "  show [name]                platforms section, secrets redacted")
	fmt.Fprintln(w, "  enable <name>              enable a gateway platform (applies automatically)")
	fmt.Fprintln(w, "  disable <name>             disable a gateway platform (applies automatically)")
	fmt.Fprintln(w, "  set discord --token REF [--require-mention BOOL] [--allow IDS] [--ban IDS]")
	fmt.Fprintln(w, "  set bluebubbles --server-url URL --server-password REF [listener flags]")
	fmt.Fprintln(w, "                             [--require-mention BOOL] [--allow IDS] [--ban IDS]")
	fmt.Fprintln(w, "                             [--mention-pattern RE ...] [--webhook-host H]")
	fmt.Fprintln(w, "                             [--webhook-port N] [--webhook-path P]")
	fmt.Fprintln(w, "                             [--webhook-callback-host H]")
	fmt.Fprintln(w, "  set api --host IP --port N")
	fmt.Fprintln(w, "    IDS are comma-separated or repeatable --allow/--ban flags")
}

func globalRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", errors.New("invalid configuration root")
	}
	return abs, nil
}

func show(root, name string, stdout, stderr io.Writer) int {
	abs, err := globalRoot(root)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	path, err := config.ConfigPathForProfile(abs, "default")
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	sub := []string{"platforms"}
	if name != "" {
		if !validPlatforms[name] {
			fmt.Fprintf(stderr, "oswald: unknown platform %q\n", name)
			return exitUsage
		}
		sub = []string{"platforms", name}
	}
	out, err := config.ShowConfigFile(path, "default", sub)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprint(stdout, out)
	return exitOK
}

// errAlready reports the flag is already in the requested state.
var errAlready = errors.New("already in requested state")

func setEnabledCmd(root, name string, want bool, stdout, stderr io.Writer) int {
	if !validPlatforms[name] {
		fmt.Fprintf(stderr, "oswald: unknown platform %q\n", name)
		platformsUsage(stderr)
		return exitUsage
	}
	effect, err := setEnabled(root, name, want)
	if err != nil {
		if errors.Is(err, errAlready) {
			if want {
				fmt.Fprintf(stdout, "platform %s already enabled\n", name)
			} else {
				fmt.Fprintf(stdout, "platform %s already disabled\n", name)
			}
			return exitOK
		}
		reportPlatformsError(stderr, err)
		return exitError
	}
	if want {
		fmt.Fprintf(stdout, "platform %s enabled (%s)\n", name, effect)
	} else {
		fmt.Fprintf(stdout, "platform %s disabled (%s)\n", name, effect)
	}
	return exitOK
}

// setEnabled flips platforms.<name>.enabled through staged validation.
func setEnabled(root, name string, want bool) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", errors.New("invalid configuration root")
	}
	path := filepath.Join(abs, "config.yaml")
	oldCfg, _ := config.Validate(abs)
	data, err := readConfigFile(path)
	if err != nil {
		return "", err
	}
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&doc); err != nil {
		return "", errors.New("configuration file is not valid YAML")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "", errors.New("configuration file is not valid YAML")
	}
	rootMap := doc.Content[0]
	platforms := mappingValue(rootMap, "platforms")
	if platforms == nil || platforms.Kind != yaml.MappingNode {
		return "", fmt.Errorf("unknown platform %q", name)
	}
	entry := mappingValue(platforms, name)
	if entry == nil || entry.Kind != yaml.MappingNode {
		return "", fmt.Errorf("unknown platform %q", name)
	}
	enabled := mappingValue(entry, "enabled")
	if enabled == nil || enabled.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("platform %q has no enabled setting", name)
	}
	if enabled.Tag == "!!bool" && ((want && enabled.Value == "true") || (!want && enabled.Value == "false")) {
		return "", errAlready
	}
	enabled.Tag = "!!bool"
	enabled.Style = 0
	if want {
		enabled.Value = "true"
	} else {
		enabled.Value = "false"
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return "", errors.New("configuration file is not valid YAML")
	}
	if err := encoder.Close(); err != nil {
		return "", errors.New("configuration file is not valid YAML")
	}
	if out.Len() > maxConfigBytes {
		return "", errors.New("configuration file exceeds size limit")
	}
	if err := config.CommitConfigCandidate(abs, path, out.Bytes(), data); err != nil {
		return "", err
	}
	return effectForChange(abs, oldCfg), nil
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

// reportPlatformsError prints a classified diagnostic without values.
func reportPlatformsError(stderr io.Writer, err error) {
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

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// readConfigFile reads the configuration file without following symlinks.
func readConfigFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("configuration file not found")
		}
		return nil, errors.New("configuration file unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("configuration file unavailable")
	}
	if info.Size() > maxConfigBytes {
		return nil, errors.New("configuration file exceeds size limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil || len(data) > maxConfigBytes {
		return nil, errors.New("configuration file unavailable")
	}
	return data, nil
}

// stringSliceFlag collects repeatable or comma-separated values.
type stringSliceFlag []string

func (f *stringSliceFlag) String() string { return strings.Join(*f, ",") }

func (f *stringSliceFlag) Set(raw string) error {
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			*f = append(*f, trimmed)
		}
	}
	return nil
}

func setFlags(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

func yamlStringList(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.WriteString("[")
	for i, v := range values {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", v)
	}
	b.WriteString("]")
	return b.String()
}

func validateBoolValue(raw string) error {
	if raw != "true" && raw != "false" {
		return fmt.Errorf("invalid boolean value %q (want true|false)", raw)
	}
	return nil
}

func validateHTTPURL(raw string) error {
	endpoint, err := url.Parse(raw)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return fmt.Errorf("invalid URL %q", raw)
	}
	return nil
}
