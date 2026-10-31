package mcp

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// resolveScope loads the configuration and selects the operator profile
// named by the --profile flag (or OSWALD_HOME when empty). Explicit flags
// contradicting OSWALD_HOME fail closed via ResolveProfile.
func resolveScope(root, flagProfile string) (profile *config.Config, profileName, profileRoot string, err error) {
	name, err := config.ResolveProfile(flagProfile)
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

// serverName normalizes a positional server argument the same way auth does:
// trimmed and lowercased, since server names are lowercase by validation.
func serverName(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// partitionArgs splits tokens into flag tokens and positionals so the server
// name is accepted in any position, mirroring the auth command. valueFlags
// take a following value (both --flag value and --flag=value forms);
// boolFlags stand alone.
func partitionArgs(args []string, valueFlags map[string]bool, boolFlags map[string]bool) (flagArgs, rest []string) {
	for i := 0; i < len(args); i++ {
		token := args[i]
		name := token
		inlineValue := false
		if j := strings.Index(token, "="); strings.HasPrefix(token, "-") && j >= 0 {
			name, inlineValue = token[:j], true
		}
		key := strings.TrimLeft(name, "-")
		if valueFlags[key] {
			flagArgs = append(flagArgs, token)
			if !inlineValue {
				if i+1 < len(args) {
					i++
					flagArgs = append(flagArgs, args[i])
				}
			}
			continue
		}
		if boolFlags[key] {
			flagArgs = append(flagArgs, token)
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

// keyValueFlag collects repeatable key=value options.
type keyValueFlag map[string]string

func (f keyValueFlag) String() string { return fmt.Sprintf("%v", map[string]string(f)) }

func (f keyValueFlag) Set(raw string) error {
	name, value, ok := strings.Cut(raw, "=")
	if !ok || strings.TrimSpace(name) == "" {
		return fmt.Errorf("must look like key=value")
	}
	f[strings.TrimSpace(name)] = strings.TrimSpace(value)
	return nil
}

// stringListFlag collects repeatable string options.
type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ",") }

func (f *stringListFlag) Set(raw string) error {
	*f = append(*f, raw)
	return nil
}

// splitCSV splits comma-separated values, dropping blanks.
func splitCSV(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// setFlags returns the flag names explicitly passed on the command line.
func setFlags(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// reportMCPError prints a classified diagnostic without values.
func reportMCPError(stderr io.Writer, err error) {
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

// profileFlag registers the shared --profile flag.
func profileFlag(fs *flag.FlagSet) *string {
	return fs.String("profile", "", "operator profile owning the MCP server")
}
