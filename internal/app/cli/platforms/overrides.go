// Channel-override management for `oswald platforms`: per-channel/per-chat
// {provider, model} routes under platforms.<name>.channel_overrides. Unlike
// the rest of the platforms group (global-only), overrides exist per profile
// (global baseline plus profile overlays), so this subcommand accepts
// --profile and fences reads/mutations to the calling scope exactly like the
// providers and model groups do.
package platforms

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/app/cli/help"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// Leaf usage lines double as `--help` pages.
const (
	channelOverridesUsage       = "usage: oswald platforms channel-overrides <list|add|edit|remove|unset> [options]"
	channelOverridesListUsage   = "usage: oswald platforms channel-overrides list [--platform discord|bluebubbles] [--profile NAME]"
	channelOverridesAddUsage    = "usage: oswald platforms channel-overrides add --platform discord|bluebubbles --channel ID --provider NAME --model MODEL [--profile NAME]"
	channelOverridesEditUsage   = "usage: oswald platforms channel-overrides edit --platform discord|bluebubbles --channel ID [--provider NAME] [--model MODEL] [--profile NAME]"
	channelOverridesRemoveUsage = "usage: oswald platforms channel-overrides remove --platform discord|bluebubbles --channel ID [--profile NAME]"
	channelOverridesUnsetUsage  = "usage: oswald platforms channel-overrides unset --platform discord|bluebubbles --channel ID [--profile NAME]"
)

// overridePlatforms bounds which platforms.<name> keys carry channel routes.
var overridePlatforms = map[string]bool{"discord": true, "bluebubbles": true}

type overrideTarget struct {
	root    string
	profile string
	path    string
}

func resolveOverrideTarget(root, profile string, mutating bool) (*overrideTarget, error) {
	var name string
	var err error
	if mutating {
		name, err = config.ResolveProfile(profile)
	} else {
		name, err = config.ResolveReadProfile(profile)
	}
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
	return &overrideTarget{root: abs, profile: name, path: path}, nil
}

// channelOverridesCmd dispatches the channel-overrides subcommand. The
// platform/channel identity is accepted in any position among flags.
func channelOverridesCmd(_ context.Context, root string, args []string, stdout, stderr io.Writer) int {
	profile, rest := splitOverrideProfile(args)
	if len(rest) == 0 {
		fmt.Fprintln(stderr, channelOverridesUsage)
		return exitUsage
	}
	switch rest[0] {
	case "list":
		if help.WantsHelp(rest[1:]) {
			fmt.Fprintln(stdout, channelOverridesListUsage)
			return exitOK
		}
		target, err := resolveOverrideTarget(root, profile, false)
		if err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
		return overridesList(target, rest[1:], stdout, stderr)
	case "add":
		if help.WantsHelp(rest[1:]) {
			fmt.Fprintln(stdout, channelOverridesAddUsage)
			return exitOK
		}
		target, err := resolveOverrideTarget(root, profile, true)
		if err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
		return overridesAdd(target, rest[1:], stdout, stderr)
	case "edit":
		if help.WantsHelp(rest[1:]) {
			fmt.Fprintln(stdout, channelOverridesEditUsage)
			return exitOK
		}
		target, err := resolveOverrideTarget(root, profile, true)
		if err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
		return overridesEdit(target, rest[1:], stdout, stderr)
	case "remove":
		if help.WantsHelp(rest[1:]) {
			fmt.Fprintln(stdout, channelOverridesRemoveUsage)
			return exitOK
		}
		target, err := resolveOverrideTarget(root, profile, true)
		if err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
		return overridesDelete(target, rest[1:], stdout, stderr, channelOverridesRemoveUsage, "removed")
	case "unset":
		if help.WantsHelp(rest[1:]) {
			fmt.Fprintln(stdout, channelOverridesUnsetUsage)
			return exitOK
		}
		target, err := resolveOverrideTarget(root, profile, true)
		if err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
		return overridesDelete(target, rest[1:], stdout, stderr, channelOverridesUnsetUsage, "unset")
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, channelOverridesUsage)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown channel-overrides command %q\n", rest[0])
		fmt.Fprintln(stderr, channelOverridesUsage)
		return exitUsage
	}
}

func overridesList(t *overrideTarget, args []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("channel-overrides list", flag.ContinueOnError)
	sub.SetOutput(stderr)
	platform := sub.String("platform", "", "discord or bluebubbles")
	flagArgs, rest := partitionOverrideArgs(args)
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 0 {
		fmt.Fprintln(stderr, channelOverridesListUsage)
		return exitUsage
	}
	if *platform != "" && !overridePlatforms[*platform] {
		fmt.Fprintf(stderr, "oswald: unknown platform %q\n", *platform)
		return exitUsage
	}
	names := []string{"discord", "bluebubbles"}
	if *platform != "" {
		names = []string{*platform}
	}
	printed := false
	for _, name := range names {
		out, err := config.ShowConfigFile(t.path, t.profile, []string{"platforms", name, "channel_overrides"})
		if err != nil {
			if strings.Contains(err.Error(), "unknown path") {
				continue
			}
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
		fmt.Fprintf(stdout, "%s:\n%s", name, indentOverrideBlock(out))
		printed = true
	}
	if !printed {
		fmt.Fprintln(stdout, "no channel overrides configured")
	}
	return exitOK
}

func indentOverrideBlock(out string) string {
	if strings.TrimSpace(out) == "" || strings.TrimSpace(out) == "{}\n" {
		return "  (none)\n"
	}
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

func overridesAdd(t *overrideTarget, args []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("channel-overrides add", flag.ContinueOnError)
	sub.SetOutput(stderr)
	platform := sub.String("platform", "", "discord or bluebubbles")
	channel := sub.String("channel", "", "channel id or chat id")
	provider := sub.String("provider", "", "provider name (custom: prefix accepted)")
	model := sub.String("model", "", "model id")
	flagArgs, rest := partitionOverrideArgs(args)
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 0 || *platform == "" || *channel == "" || *provider == "" || *model == "" {
		fmt.Fprintln(stderr, channelOverridesAddUsage)
		return exitUsage
	}
	if !overridePlatforms[*platform] {
		fmt.Fprintf(stderr, "oswald: unknown platform %q\n", *platform)
		return exitUsage
	}
	id := strings.TrimSpace(*channel)
	if id == "" || !utf8.ValidString(id) || len([]rune(id)) > 256 {
		fmt.Fprintln(stderr, "oswald: invalid --channel value")
		return exitUsage
	}
	name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(*provider), "custom:"))
	if name == "" {
		fmt.Fprintln(stderr, "oswald: invalid --provider value")
		return exitUsage
	}
	if value := strings.TrimSpace(*model); value == "" || !utf8.ValidString(value) || len([]rune(value)) > 256 {
		fmt.Fprintln(stderr, "oswald: invalid --model value")
		return exitUsage
	}
	raw := fmt.Sprintf("{provider: %q, model: %q}", "custom:"+name, strings.TrimSpace(*model))
	oldCfg, _ := config.Validate(t.root)
	if err := config.SetConfigValue(t.root, t.profile, []string{"platforms", *platform, "channel_overrides", id}, raw); err != nil {
		reportPlatformsError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "set channel override %s/%s (%s)\n", *platform, id, effectForChange(t.root, oldCfg))
	return exitOK
}

// overridesEdit changes one field of an existing per-channel route in place.
// At least one of --provider/--model is required; omitted leaves inherit the
// entry's current values. The write lands in the calling scope, so editing an
// inherited global entry from a named profile creates a profile overlay.
func overridesEdit(t *overrideTarget, args []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("channel-overrides edit", flag.ContinueOnError)
	sub.SetOutput(stderr)
	platform := sub.String("platform", "", "discord or bluebubbles")
	channel := sub.String("channel", "", "channel id or chat id")
	provider := sub.String("provider", "", "provider name (custom: prefix accepted)")
	model := sub.String("model", "", "model id")
	flagArgs, rest := partitionOverrideArgs(args)
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 0 || *platform == "" || *channel == "" || (*provider == "" && *model == "") {
		fmt.Fprintln(stderr, channelOverridesEditUsage)
		return exitUsage
	}
	if !overridePlatforms[*platform] {
		fmt.Fprintf(stderr, "oswald: unknown platform %q\n", *platform)
		return exitUsage
	}
	id := strings.TrimSpace(*channel)
	if id == "" || !utf8.ValidString(id) || len([]rune(id)) > 256 {
		fmt.Fprintln(stderr, "oswald: invalid --channel value")
		return exitUsage
	}
	gateway := *platform
	if *platform == "bluebubbles" {
		gateway = "imessage"
	}
	cfg, err := config.Validate(t.root)
	if err != nil {
		reportPlatformsError(stderr, err)
		return exitError
	}
	active := cfg
	if t.profile != "default" {
		active = cfg.Profiles[t.profile]
		if active == nil {
			fmt.Fprintln(stderr, "oswald: unknown profile")
			return exitError
		}
	}
	var current config.ChannelOverride
	if active.ChannelOverrides != nil {
		var ok bool
		current, ok = active.ChannelOverrides[gateway][id]
		if !ok {
			fmt.Fprintf(stderr, "oswald: unknown channel override %s/%s\n", *platform, id)
			return exitError
		}
	} else {
		fmt.Fprintf(stderr, "oswald: unknown channel override %s/%s\n", *platform, id)
		return exitError
	}
	providerName := current.Provider
	if *provider != "" {
		providerName = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(*provider), "custom:"))
		if providerName == "" {
			fmt.Fprintln(stderr, "oswald: invalid --provider value")
			return exitUsage
		}
	}
	modelID := current.Model
	if *model != "" {
		modelID = strings.TrimSpace(*model)
		if modelID == "" || !utf8.ValidString(modelID) || len([]rune(modelID)) > 256 {
			fmt.Fprintln(stderr, "oswald: invalid --model value")
			return exitUsage
		}
	}
	raw := fmt.Sprintf("{provider: %q, model: %q}", "custom:"+providerName, modelID)
	if err := config.SetConfigValue(t.root, t.profile, []string{"platforms", *platform, "channel_overrides", id}, raw); err != nil {
		reportPlatformsError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "edited channel override %s/%s (%s)\n", *platform, id, effectForChange(t.root, cfg))
	return exitOK
}

// overridesDelete removes one per-channel route from the calling scope. It
// backs both `remove` and its `unset` alias; usage names the invoked verb.
func overridesDelete(t *overrideTarget, args []string, stdout, stderr io.Writer, usage, verb string) int {
	sub := flag.NewFlagSet("channel-overrides "+verb, flag.ContinueOnError)
	sub.SetOutput(stderr)
	platform := sub.String("platform", "", "discord or bluebubbles")
	channel := sub.String("channel", "", "channel id or chat id")
	flagArgs, rest := partitionOverrideArgs(args)
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 0 || *platform == "" || *channel == "" {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	if !overridePlatforms[*platform] {
		fmt.Fprintf(stderr, "oswald: unknown platform %q\n", *platform)
		return exitUsage
	}
	id := strings.TrimSpace(*channel)
	if id == "" {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	oldCfg, _ := config.Validate(t.root)
	if err := config.UnsetConfigValue(t.root, t.profile, []string{"platforms", *platform, "channel_overrides", id}); err != nil {
		reportPlatformsError(stderr, err)
		return exitError
	}
	if verb == "unset" {
		fmt.Fprintf(stdout, "unset channel override %s/%s (%s)\n", *platform, id, effectForChange(t.root, oldCfg))
		return exitOK
	}
	fmt.Fprintf(stdout, "removed channel override %s/%s (%s)\n", *platform, id, effectForChange(t.root, oldCfg))
	return exitOK
}

// splitOverrideProfile extracts --profile NAME / --profile=NAME in any
// position, returning the profile value and remaining args.
func splitOverrideProfile(args []string) (string, []string) {
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

// partitionOverrideArgs splits value flags from positionals so identity flags
// are accepted in any position.
func partitionOverrideArgs(args []string) (flagArgs, rest []string) {
	valueFlags := map[string]bool{"profile": true, "platform": true, "channel": true, "provider": true, "model": true}
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
