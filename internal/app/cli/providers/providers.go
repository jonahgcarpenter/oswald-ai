// Package providers implements the `oswald providers` command group:
// explicit, operator-driven management of the top-level providers map. All
// mutation goes through the mediated core (staged validation before write,
// last-known-good, atomic replace).
package providers

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
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

// Run dispatches the providers command group.
func Run(_ context.Context, root string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("providers", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profileFlag := fs.String("profile", "", "operator profile owning the configuration")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	if len(rest) == 0 {
		providersUsage(stderr)
		return exitUsage
	}
	target, err := resolveTarget(root, *profileFlag)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	switch rest[0] {
	case "list":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "usage: oswald providers list [--profile NAME]")
			return exitUsage
		}
		return show(target, nil, stdout, stderr)
	case "show":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "usage: oswald providers show <name> [--profile NAME]")
			return exitUsage
		}
		return show(target, []string{"providers", rest[1]}, stdout, stderr)
	case "add":
		return addCmd(target, rest[1:], stdout, stderr)
	case "remove":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "usage: oswald providers remove <name> [--profile NAME]")
			return exitUsage
		}
		return remove(target, rest[1], stdout, stderr)
	case "help", "-h", "--help":
		providersUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown providers command %q\n", rest[0])
		providersUsage(stderr)
		return exitUsage
	}
}

func providersUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald providers [--profile NAME] <list|show|add|remove> [options]")
	fmt.Fprintln(w, "  list                  provider names with api host, key redacted")
	fmt.Fprintln(w, "  show <name>           one provider entry, key redacted")
	fmt.Fprintln(w, "  add <name> --api URL [--key REF]")
	fmt.Fprintln(w, "                        add a provider through staged validation")
	fmt.Fprintln(w, "  remove <name>         remove a provider not referenced by model.provider")
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

func show(t *target, sub []string, stdout, stderr io.Writer) int {
	out, err := config.ShowConfigFile(t.path, t.profile, sub)
	if err != nil {
		// Empty named-profile override reads as missing section: report no
		// providers rather than a hard error for list.
		if sub == nil && strings.Contains(err.Error(), "unknown path") {
			fmt.Fprintln(stdout, "no providers configured")
			return exitOK
		}
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprint(stdout, out)
	return exitOK
}

func addCmd(t *target, args []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("providers add", flag.ContinueOnError)
	sub.SetOutput(stderr)
	api := sub.String("api", "", "provider base URL (https://host[:port][/prefix])")
	key := sub.String("key", "", "credential literal or ${VAR} reference")
	flagArgs, rest := partitionAddArgs(args)
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 1 || *api == "" {
		fmt.Fprintln(stderr, "usage: oswald providers add <name> --api URL [--key REF] [--profile NAME]")
		return exitUsage
	}
	name := rest[0]
	if _, err := config.ShowConfigFile(t.path, t.profile, []string{"providers", name}); err == nil {
		fmt.Fprintf(stderr, "oswald: provider %q already exists\n", name)
		return exitError
	}
	if err := validateProviderAPI(*api); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	oldCfg, _ := config.Validate(t.root)
	if err := config.SetConfigValue(t.root, t.profile, []string{"providers", name, "api"}, *api); err != nil {
		reportProvidersError(stderr, err)
		return exitError
	}
	if *key != "" {
		if err := config.SetConfigValue(t.root, t.profile, []string{"providers", name, "key"}, *key); err != nil {
			reportProvidersError(stderr, err)
			return exitError
		}
	}
	fmt.Fprintf(stdout, "provider %s added (%s)\n", name, effectForChange(t.root, oldCfg))
	return exitOK
}

func remove(t *target, name string, stdout, stderr io.Writer) int {
	oldCfg, _ := config.Validate(t.root)
	// Refuse to remove the provider backing model.provider for the target
	// profile; staged validation remains authoritative for cross-profile
	// references (global providers back named profiles).
	if current, err := config.Validate(t.root); err == nil {
		var provider string
		if t.profile == "default" {
			provider = current.LLMGatewayProvider
		} else if p, ok := current.Profiles[t.profile]; ok && p != nil {
			provider = p.LLMGatewayProvider
		}
		if provider == name {
			fmt.Fprintf(stderr, "oswald: provider %q is referenced by model.provider\n", name)
			return exitError
		}
	}
	if err := config.UnsetConfigValue(t.root, t.profile, []string{"providers", name}); err != nil {
		// Unsetting a whole provider that leaves an empty map may fail
		// validation if model still references it; surface the diagnostic.
		reportProvidersError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "provider %s removed (%s)\n", name, effectForChange(t.root, oldCfg))
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
	return "restart required to take effect"
}

// reportProvidersError prints a classified diagnostic without values.
func reportProvidersError(stderr io.Writer, err error) {
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

// validateProviderAPI mirrors startup endpoint rules so unused providers are
// still well-formed at the CLI boundary (staged validation only checks the
// selected provider).
func validateProviderAPI(raw string) error {
	endpoint, err := url.Parse(raw)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return fmt.Errorf("invalid provider api URL %q", raw)
	}
	return nil
}

// partitionAddArgs splits tokens so the provider name is accepted in any
// position. Value flags take a following value (both --flag value and
// --flag=value forms).
func partitionAddArgs(args []string) (flagArgs, rest []string) {
	valueFlags := map[string]bool{"profile": true, "api": true, "key": true}
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
