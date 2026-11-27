package providers

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// editCmd changes one provider's leaves in place through the mediated core
// (staged validation before write, last-known-good, atomic replace). At
// least one leaf flag is required; --clear-key drops the static key, which
// is required when moving a provider to a subscription auth kind.
func editCmd(t *target, args []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("providers edit", flag.ContinueOnError)
	sub.SetOutput(stderr)
	api := sub.String("api", "", "provider base URL (https://host[:port][/prefix])")
	key := sub.String("key", "", "credential literal or ${VAR} reference")
	auth := sub.String("auth", "", "auth kind: key|chatgpt-oauth|claude-sdk")
	clearKey := sub.Bool("clear-key", false, "remove the static key")
	clearAPI := sub.Bool("clear-api", false, "remove the api base URL")
	flagArgs, rest := partitionEditArgs(args)
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 1 || (*api == "" && *key == "" && *auth == "" && !*clearKey && !*clearAPI) {
		fmt.Fprintln(stderr, editUsage)
		return exitUsage
	}
	name := rest[0]
	if _, err := config.ShowConfigFile(t.path, t.profile, []string{"providers", name}); err != nil {
		fmt.Fprintf(stderr, "oswald: unknown provider %q\n", name)
		return exitError
	}
	kind := config.NormalizeProviderAuth(*auth)
	if *auth != "" && kind == "" {
		fmt.Fprintln(stderr, "oswald: invalid --auth value")
		return exitUsage
	}
	if kind != "" && kind != config.ProviderAuthKey && *key != "" {
		fmt.Fprintln(stderr, "oswald: subscription providers do not take --key")
		return exitUsage
	}
	if *api != "" {
		if err := validateProviderAPI(*api); err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
	}
	oldCfg, _ := config.Validate(t.root)
	sets := map[string]string{}
	var unsets []string
	if *key != "" {
		sets["providers."+name+".key"] = *key
	}
	if *api != "" {
		sets["providers."+name+".api"] = *api
	}
	if kind != "" {
		sets["providers."+name+".auth"] = kind
	}
	if *clearKey {
		unsets = append(unsets, "providers."+name+".key")
	}
	if *clearAPI {
		unsets = append(unsets, "providers."+name+".api")
	}
	if err := config.SetConfigLeaves(t.root, t.profile, sets, unsets); err != nil {
		reportProvidersError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "provider %s updated (%s)\n", name, effectForChange(t.root, oldCfg))
	return exitOK
}

// partitionEditArgs splits tokens so the provider name is accepted in any
// position. Value flags take a following value (both --flag value and
// --flag=value forms); --clear-key is boolean.
func partitionEditArgs(args []string) (flagArgs, rest []string) {
	valueFlags := map[string]bool{"profile": true, "api": true, "key": true, "auth": true}
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
