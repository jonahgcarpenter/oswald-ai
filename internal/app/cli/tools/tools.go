// Package tools implements the `oswald tools` command group: explicit,
// operator-driven reads and writes of the top-level tools section. All
// mutation goes through the mediated core (staged validation before write,
// last-known-good, atomic replace). Secrets are redacted in all output.
package tools

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

// backendAliases maps CLI names to YAML keys.
var backendAliases = map[string]string{
	"web-search":     "web_search",
	"web_search":     "web_search",
	"image":          "image_generate",
	"image_generate": "image_generate",
	"browser":        "browser",
}

// Run dispatches the tools command group. --profile is accepted in any
// position.
func Run(_ context.Context, root string, args []string, stdout, stderr io.Writer) int {
	profile, rest := splitProfile(args)
	if len(rest) == 0 {
		toolsUsage(stderr)
		return exitUsage
	}
	target, err := resolveTarget(root, profile)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	switch rest[0] {
	case "show":
		if len(rest) > 2 {
			fmt.Fprintln(stderr, "usage: oswald tools show [web-search|image|browser] [--profile NAME]")
			return exitUsage
		}
		backend := ""
		if len(rest) == 2 {
			backend = rest[1]
		}
		return show(target, backend, stdout, stderr)
	case "set":
		return setCmd(target, rest[1:], stdout, stderr)
	case "unset":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "usage: oswald tools unset <backend.field> [--profile NAME]")
			return exitUsage
		}
		return unsetField(target, rest[1], stdout, stderr)
	case "help", "-h", "--help":
		toolsUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown tools command %q\n", rest[0])
		toolsUsage(stderr)
		return exitUsage
	}
}

func toolsUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald tools [--profile NAME] <show|set|unset> [options]")
	fmt.Fprintln(w, "  show [web-search|image|browser]")
	fmt.Fprintln(w, "                        tools section, secrets redacted")
	fmt.Fprintln(w, "  set web-search --brave-key REF --searxng-url URL")
	fmt.Fprintln(w, "  set image --url URL --generation-timeout DUR")
	fmt.Fprintln(w, "  set browser [browser flags]")
	fmt.Fprintln(w, "                        staged-validate and atomically write one backend")
	fmt.Fprintln(w, "  unset <backend.field>  clear one optional tools leaf")
	fmt.Fprintln(w, "    browser flags: --launch-command CMD --endpoint-url URL")
	fmt.Fprintln(w, "      --command-timeout DUR --snapshot-threshold N")
	fmt.Fprintln(w, "      --inactivity-timeout DUR --record-sessions BOOL")
	fmt.Fprintln(w, "      --allow-private-urls BOOL --allow-insecure-urls BOOL")
	fmt.Fprintln(w, "      --restrict-evaluate BOOL")
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

func show(t *target, backend string, stdout, stderr io.Writer) int {
	sub := []string{"tools"}
	if backend != "" {
		key, ok := backendAliases[backend]
		if !ok {
			fmt.Fprintf(stderr, "oswald: unknown tools backend %q\n", backend)
			return exitUsage
		}
		sub = []string{"tools", key}
	}
	out, err := config.ShowConfigFile(t.path, t.profile, sub)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprint(stdout, out)
	return exitOK
}

func setCmd(t *target, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: oswald tools set <web-search|image|browser> [flags] [--profile NAME]")
		return exitUsage
	}
	// Backend name accepted in any position.
	flagArgs, rest := partitionToolsArgs(args)
	backend := ""
	filtered := rest[:0:0]
	for _, tok := range rest {
		if backend == "" {
			if _, ok := backendAliases[tok]; ok {
				backend = tok
				continue
			}
		}
		filtered = append(filtered, tok)
	}
	_ = filtered
	if backend == "" {
		// Fall back: first positional is the backend even if unknown, so the
		// error names it precisely.
		if len(rest) > 0 {
			fmt.Fprintf(stderr, "oswald: unknown tools backend %q\n", rest[0])
		} else {
			fmt.Fprintln(stderr, "usage: oswald tools set <web-search|image|browser> [flags] [--profile NAME]")
		}
		return exitUsage
	}
	if len(filtered) > 0 {
		fmt.Fprintln(stderr, "usage: oswald tools set <web-search|image|browser> [flags] [--profile NAME]")
		return exitUsage
	}
	yamlBackend := backendAliases[backend]
	switch yamlBackend {
	case "web_search":
		return setWebSearch(t, flagArgs, stdout, stderr)
	case "image_generate":
		return setImage(t, flagArgs, stdout, stderr)
	case "browser":
		return setBrowser(t, flagArgs, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "oswald: unknown tools backend %q\n", backend)
		return exitUsage
	}
}

func setWebSearch(t *target, flagArgs []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("tools set web-search", flag.ContinueOnError)
	sub.SetOutput(stderr)
	braveKey := sub.String("brave-key", "", "credential literal or ${VAR} reference")
	searxngURL := sub.String("searxng-url", "", "SearXNG base URL")
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if sub.NArg() != 0 || (*braveKey == "" && *searxngURL == "") {
		fmt.Fprintln(stderr, "usage: oswald tools set web-search [--brave-key REF] [--searxng-url URL] [--profile NAME]")
		return exitUsage
	}
	set := setFlags(sub)
	oldCfg, _ := config.Validate(t.root)
	if set["brave-key"] {
		if err := config.SetConfigValue(t.root, t.profile, []string{"tools", "web_search", "brave_key"}, *braveKey); err != nil {
			reportToolsError(stderr, err)
			return exitError
		}
	}
	if set["searxng-url"] {
		if err := config.SetConfigValue(t.root, t.profile, []string{"tools", "web_search", "searxng_url"}, *searxngURL); err != nil {
			reportToolsError(stderr, err)
			return exitError
		}
	}
	fmt.Fprintf(stdout, "set tools.web_search (%s)\n", effectForChange(t.root, oldCfg))
	return exitOK
}

func setImage(t *target, flagArgs []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("tools set image", flag.ContinueOnError)
	sub.SetOutput(stderr)
	url := sub.String("url", "", "ComfyUI base URL")
	timeout := sub.String("generation-timeout", "", "generation bound, e.g. 2m")
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if sub.NArg() != 0 || (*url == "" && *timeout == "") {
		fmt.Fprintln(stderr, "usage: oswald tools set image [--url URL] [--generation-timeout DUR] [--profile NAME]")
		return exitUsage
	}
	set := setFlags(sub)
	oldCfg, _ := config.Validate(t.root)
	if set["url"] {
		if err := config.SetConfigValue(t.root, t.profile, []string{"tools", "image_generate", "url"}, *url); err != nil {
			reportToolsError(stderr, err)
			return exitError
		}
	}
	if set["generation-timeout"] {
		if err := config.SetConfigValue(t.root, t.profile, []string{"tools", "image_generate", "generation_timeout"}, *timeout); err != nil {
			reportToolsError(stderr, err)
			return exitError
		}
	}
	fmt.Fprintf(stdout, "set tools.image_generate (%s)\n", effectForChange(t.root, oldCfg))
	return exitOK
}

func setBrowser(t *target, flagArgs []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("tools set browser", flag.ContinueOnError)
	sub.SetOutput(stderr)
	launchCommand := sub.String("launch-command", "", "browser launch command")
	endpointURL := sub.String("endpoint-url", "", "browser endpoint URL")
	commandTimeout := sub.String("command-timeout", "", "command bound, e.g. 30s")
	snapshotThreshold := sub.String("snapshot-threshold", "", "snapshot rune threshold")
	inactivityTimeout := sub.String("inactivity-timeout", "", "inactivity bound, e.g. 2m")
	recordSessions := sub.String("record-sessions", "", "true|false")
	allowPrivate := sub.String("allow-private-urls", "", "true|false")
	allowInsecure := sub.String("allow-insecure-urls", "", "true|false")
	restrictEvaluate := sub.String("restrict-evaluate", "", "true|false")
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if sub.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: oswald tools set browser [flags] [--profile NAME]")
		return exitUsage
	}
	set := setFlags(sub)
	if len(set) == 0 {
		fmt.Fprintln(stderr, "usage: oswald tools set browser [flags] [--profile NAME]")
		return exitUsage
	}
	for _, key := range []string{"record-sessions", "allow-private-urls", "allow-insecure-urls", "restrict-evaluate"} {
		if set[key] {
			if err := validateBoolFlag(sub, key); err != nil {
				fmt.Fprintf(stderr, "oswald: %v\n", err)
				return exitUsage
			}
		}
	}
	oldCfg, _ := config.Validate(t.root)
	leaves := map[string]string{
		"launch-command": *launchCommand, "endpoint-url": *endpointURL,
		"command-timeout": *commandTimeout, "snapshot-threshold": *snapshotThreshold,
		"inactivity-timeout": *inactivityTimeout, "record-sessions": *recordSessions,
		"allow-private-urls": *allowPrivate, "allow-insecure-urls": *allowInsecure,
		"restrict-evaluate": *restrictEvaluate,
	}
	yamlKeys := map[string]string{
		"launch-command": "launch_command", "endpoint-url": "endpoint_url",
		"command-timeout": "command_timeout", "snapshot-threshold": "snapshot_threshold",
		"inactivity-timeout": "inactivity_timeout", "record-sessions": "record_sessions",
		"allow-private-urls": "allow_private_urls", "allow-insecure-urls": "allow_insecure_urls",
		"restrict-evaluate": "restrict_evaluate",
	}
	for flagName, raw := range leaves {
		if !set[flagName] {
			continue
		}
		if err := config.SetConfigValue(t.root, t.profile, []string{"tools", "browser", yamlKeys[flagName]}, raw); err != nil {
			reportToolsError(stderr, err)
			return exitError
		}
	}
	fmt.Fprintf(stdout, "set tools.browser (%s)\n", effectForChange(t.root, oldCfg))
	return exitOK
}

func unsetField(t *target, dotted string, stdout, stderr io.Writer) int {
	parts := strings.Split(dotted, ".")
	if len(parts) != 2 {
		fmt.Fprintln(stderr, "usage: oswald tools unset <backend.field> [--profile NAME]")
		fmt.Fprintln(stderr, "  backends: web-search, image, browser; fields use hyphens, e.g. browser.launch-command")
		return exitUsage
	}
	backend, ok := backendAliases[parts[0]]
	if !ok {
		fmt.Fprintf(stderr, "oswald: unknown tools backend %q\n", parts[0])
		return exitUsage
	}
	field := strings.ReplaceAll(parts[1], "-", "_")
	oldCfg, _ := config.Validate(t.root)
	if err := config.UnsetConfigValue(t.root, t.profile, []string{"tools", backend, field}); err != nil {
		reportToolsError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "unset tools.%s.%s (%s)\n", backend, field, effectForChange(t.root, oldCfg))
	return exitOK
}

func isSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func setFlags(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

func validateBoolFlag(fs *flag.FlagSet, name string) error {
	var raw string
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			raw = f.Value.String()
		}
	})
	if raw != "true" && raw != "false" {
		return fmt.Errorf("invalid value for --%s: %q (want true|false)", name, raw)
	}
	return nil
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

// partitionToolsArgs splits tokens so the backend name is accepted in any
// position. Flags take a following value unless given as --flag=value.
func partitionToolsArgs(args []string) (flagArgs, rest []string) {
	for i := 0; i < len(args); i++ {
		token := args[i]
		if !strings.HasPrefix(token, "-") {
			rest = append(rest, token)
			continue
		}
		flagArgs = append(flagArgs, token)
		if strings.Contains(token, "=") {
			continue
		}
		if i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return flagArgs, rest
}

func effectForChange(root string, oldCfg *config.Config) string {
	if oldCfg != nil {
		if nextCfg, err := config.Validate(root); err == nil {
			if config.ReloadEffect(oldCfg, nextCfg) == "live" {
				return "applied live"
			}
		}
	}
	return "restart required to take effect"
}

// reportToolsError prints a classified diagnostic without values.
func reportToolsError(stderr io.Writer, err error) {
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
