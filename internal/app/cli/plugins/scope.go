package plugins

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/plugins"
)

// splitProfile extracts --profile NAME / --profile=NAME in any position.
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

// resolveScope loads config and selects the profile for reads (any existing)
// or mutations (calling scope only, contradictions fail closed).
func resolveScope(root, flagProfile string, mutating bool) (*config.Config, string, string, error) {
	var name string
	var err error
	if mutating {
		name, err = config.ResolveProfile(flagProfile)
	} else {
		name, err = config.ResolveReadProfile(flagProfile)
	}
	if err != nil {
		return nil, "", "", err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, "", "", fmt.Errorf("invalid configuration root")
	}
	cfg, err := config.LoadProfiles(abs)
	if err != nil {
		return nil, "", "", err
	}
	if name == "default" {
		return cfg, name, cfg.ProfileRoot, nil
	}
	selected, ok := cfg.Profiles[name]
	if !ok {
		return nil, "", "", fmt.Errorf("unknown profile %q", name)
	}
	return selected, name, selected.ProfileRoot, nil
}

// discoverForList scans shared + opt-in project plugins for one global root.
func discoverForList(globalRoot string) map[string]plugins.Manifest {
	userDir := plugins.UserPluginsDir(globalRoot)
	projectDir := ""
	projectEnabled := plugins.ProjectPluginsEnabled()
	if projectEnabled {
		if cwd, err := os.Getwd(); err == nil {
			projectDir = filepath.Join(cwd, ".oswald", "plugins")
		} else {
			projectEnabled = false
		}
	}
	manifests := plugins.CollectManifests("", userDir, projectDir, projectEnabled)
	return plugins.ResolveWinners(manifests)
}

func absCwd() (string, error) { return os.Getwd() }

func globalRootFor(profileRoot, profile string) string {
	if globalRoot, err := config.GlobalRootForProfile(profileRoot, profile); err == nil {
		return globalRoot
	}
	return profileRoot
}

func reportPluginsError(stderr io.Writer, err error) {
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
