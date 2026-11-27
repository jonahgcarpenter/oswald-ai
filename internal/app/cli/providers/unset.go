package providers

import (
	"fmt"
	"io"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// unsetCmd clears one leaf (key, api, or auth) of an existing provider
// through the mediated core (staged validation before write, last-known-good,
// atomic replace). It is sugar over `edit --clear-*` for the single-leaf
// case: `unset <name> key` drops the static key, `unset <name> api` drops
// the endpoint, `unset <name> auth` drops the auth kind back to the key
// default. Removing the whole provider stays `remove`.
func unsetCmd(t *target, args []string, stdout, stderr io.Writer) int {
	var positional []string
	for _, token := range args {
		if strings.HasPrefix(token, "-") {
			fmt.Fprintln(stderr, unsetUsage)
			return exitUsage
		}
		positional = append(positional, token)
	}
	if len(positional) != 2 {
		fmt.Fprintln(stderr, unsetUsage)
		return exitUsage
	}
	name := strings.TrimSpace(positional[0])
	var leaf string
	switch strings.ToLower(strings.TrimSpace(positional[1])) {
	case "key":
		leaf = "key"
	case "api":
		leaf = "api"
	case "auth":
		leaf = "auth"
	default:
		fmt.Fprintln(stderr, unsetUsage)
		return exitUsage
	}
	if name == "" {
		fmt.Fprintln(stderr, unsetUsage)
		return exitUsage
	}
	if _, err := config.ShowConfigFile(t.path, t.profile, []string{"providers", name}); err != nil {
		fmt.Fprintf(stderr, "oswald: unknown provider %q\n", name)
		return exitError
	}
	oldCfg, _ := config.Validate(t.root)
	if err := config.UnsetConfigValue(t.root, t.profile, []string{"providers", name, leaf}); err != nil {
		reportProvidersError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "unset provider %s %s (%s)\n", name, leaf, effectForChange(t.root, oldCfg))
	return exitOK
}
