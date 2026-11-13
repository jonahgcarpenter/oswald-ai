package skills

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

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

// scope resolves the target profile root for reads (calling scope only,
// contradictions fail closed) or
// mutations (calling scope only, contradictions fail closed). Skills are
// per-profile files, so on-disk existence is sufficient; routing is not
// required.
func resolveScope(root, flagProfile string, mutating bool) (string, string, error) {
	var name string
	var err error
	if mutating {
		name, err = config.ResolveProfile(flagProfile)
	} else {
		name, err = config.ResolveReadProfile(flagProfile)
	}
	if err != nil {
		return "", "", err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("invalid configuration root")
	}
	path, err := config.ConfigPathForProfile(abs, name)
	if err != nil {
		return "", "", err
	}
	return name, filepath.Dir(path), nil
}

func reportSkillsError(stderr io.Writer, err error) {
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
