package platforms

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// setCmd dispatches platforms set <platform> [flags].
func setCmd(root string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: oswald platforms set <discord|bluebubbles|api> [flags]")
		return exitUsage
	}
	// Platform name accepted in any position among flags.
	platform := ""
	var rest []string
	for _, tok := range args {
		if platform == "" && validPlatforms[tok] {
			platform = tok
			continue
		}
		rest = append(rest, tok)
	}
	// Also handle flags-first ordering: partition remaining flags.
	flagArgs, positionals := partitionSetArgs(rest)
	if platform == "" {
		if len(positionals) > 0 {
			platform = positionals[0]
			positionals = positionals[1:]
		}
	}
	if !validPlatforms[platform] {
		if platform == "" {
			fmt.Fprintln(stderr, "usage: oswald platforms set <discord|bluebubbles|api> [flags]")
		} else {
			fmt.Fprintf(stderr, "oswald: unknown platform %q\n", platform)
		}
		return exitUsage
	}
	if len(positionals) > 0 {
		fmt.Fprintln(stderr, "usage: oswald platforms set <discord|bluebubbles|api> [flags]")
		return exitUsage
	}
	switch platform {
	case "discord":
		return setDiscord(root, flagArgs, stdout, stderr)
	case "bluebubbles":
		return setBlueBubbles(root, flagArgs, stdout, stderr)
	case "api":
		return setAPI(root, flagArgs, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "oswald: unknown platform %q\n", platform)
		return exitUsage
	}
}

// partitionSetArgs forwards flags (with values) and collects positionals.
func partitionSetArgs(args []string) (flagArgs, rest []string) {
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

func setDiscord(root string, flagArgs []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("platforms set discord", flag.ContinueOnError)
	sub.SetOutput(stderr)
	token := sub.String("token", "", "credential literal or ${VAR} reference")
	requireMention := sub.String("require-mention", "", "true|false")
	var allow, ban stringSliceFlag
	sub.Var(&allow, "allow", "allowed identity (repeatable or comma-separated)")
	sub.Var(&ban, "ban", "banned identity (repeatable or comma-separated)")
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if sub.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: oswald platforms set discord [--token REF] [--require-mention BOOL] [--allow IDS] [--ban IDS]")
		return exitUsage
	}
	set := setFlags(sub)
	if len(set) == 0 {
		fmt.Fprintln(stderr, "usage: oswald platforms set discord [--token REF] [--require-mention BOOL] [--allow IDS] [--ban IDS]")
		return exitUsage
	}
	if set["require-mention"] {
		if err := validateBoolValue(*requireMention); err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitUsage
		}
	}
	abs, err := globalRoot(root)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	oldCfg, _ := config.Validate(abs)
	if set["token"] {
		if err := config.SetConfigValue(abs, "default", []string{"platforms", "discord", "extra", "token"}, *token); err != nil {
			reportPlatformsError(stderr, err)
			return exitError
		}
	}
	if set["require-mention"] {
		if err := config.SetConfigValue(abs, "default", []string{"platforms", "discord", "extra", "require_mention"}, *requireMention); err != nil {
			reportPlatformsError(stderr, err)
			return exitError
		}
	}
	if len(allow) > 0 {
		if err := config.SetConfigValue(abs, "default", []string{"platforms", "discord", "extra", "allowed_users"}, yamlStringList(allow)); err != nil {
			reportPlatformsError(stderr, err)
			return exitError
		}
	}
	if len(ban) > 0 {
		if err := config.SetConfigValue(abs, "default", []string{"platforms", "discord", "extra", "banned_users"}, yamlStringList(ban)); err != nil {
			reportPlatformsError(stderr, err)
			return exitError
		}
	}
	fmt.Fprintf(stdout, "set platforms.discord.extra (%s)\n", effectForChange(abs, oldCfg))
	return exitOK
}

func setBlueBubbles(root string, flagArgs []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("platforms set bluebubbles", flag.ContinueOnError)
	sub.SetOutput(stderr)
	serverURL := sub.String("server-url", "", "BlueBubbles server URL")
	serverPassword := sub.String("server-password", "", "credential literal or ${VAR} reference")
	webhookHost := sub.String("webhook-host", "", "webhook bind host (0.0.0.0)")
	webhookPort := sub.String("webhook-port", "", "webhook bind port")
	webhookPath := sub.String("webhook-path", "", "webhook route path")
	webhookCallbackHost := sub.String("webhook-callback-host", "", "LAN-reachable callback host")
	requireMention := sub.String("require-mention", "", "true|false")
	var allow, ban stringSliceFlag
	var mention stringSliceFlag
	sub.Var(&allow, "allow", "allowed identity (repeatable or comma-separated)")
	sub.Var(&ban, "ban", "banned identity (repeatable or comma-separated)")
	sub.Var(&mention, "mention-pattern", "mention regex (repeatable or comma-separated)")
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if sub.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: oswald platforms set bluebubbles [flags]")
		return exitUsage
	}
	set := setFlags(sub)
	if len(set) == 0 {
		fmt.Fprintln(stderr, "usage: oswald platforms set bluebubbles [flags]")
		return exitUsage
	}
	if set["server-url"] {
		if err := validateHTTPURL(*serverURL); err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
	}
	if set["require-mention"] {
		if err := validateBoolValue(*requireMention); err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitUsage
		}
	}
	abs, err := globalRoot(root)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	oldCfg, _ := config.Validate(abs)
	write := func(leaf, raw string) error {
		return config.SetConfigValue(abs, "default", []string{"platforms", "bluebubbles", "extra", leaf}, raw)
	}
	if set["server-url"] {
		if err := write("server_url", *serverURL); err != nil {
			reportPlatformsError(stderr, err)
			return exitError
		}
	}
	if set["server-password"] {
		if err := write("server_password", *serverPassword); err != nil {
			reportPlatformsError(stderr, err)
			return exitError
		}
	}
	if set["webhook-host"] {
		if err := write("webhook_host", *webhookHost); err != nil {
			reportPlatformsError(stderr, err)
			return exitError
		}
	}
	if set["webhook-port"] {
		if err := write("webhook_port", *webhookPort); err != nil {
			reportPlatformsError(stderr, err)
			return exitError
		}
	}
	if set["webhook-path"] {
		if err := write("webhook_path", *webhookPath); err != nil {
			reportPlatformsError(stderr, err)
			return exitError
		}
	}
	if set["webhook-callback-host"] {
		if err := write("webhook_callback_host", *webhookCallbackHost); err != nil {
			reportPlatformsError(stderr, err)
			return exitError
		}
	}
	if set["require-mention"] {
		if err := write("require_mention", *requireMention); err != nil {
			reportPlatformsError(stderr, err)
			return exitError
		}
	}
	if len(allow) > 0 {
		if err := write("allowed_users", yamlStringList(allow)); err != nil {
			reportPlatformsError(stderr, err)
			return exitError
		}
	}
	if len(ban) > 0 {
		if err := write("banned_users", yamlStringList(ban)); err != nil {
			reportPlatformsError(stderr, err)
			return exitError
		}
	}
	if len(mention) > 0 {
		if err := write("mention_pattern", yamlStringList(mention)); err != nil {
			reportPlatformsError(stderr, err)
			return exitError
		}
	}
	fmt.Fprintf(stdout, "set platforms.bluebubbles.extra (%s)\n", effectForChange(abs, oldCfg))
	return exitOK
}

func setAPI(root string, flagArgs []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("platforms set api", flag.ContinueOnError)
	sub.SetOutput(stderr)
	host := sub.String("host", "", "listener host (127.0.0.1 or 0.0.0.0)")
	port := sub.String("port", "", "listener port")
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if sub.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: oswald platforms set api [--host IP] [--port N]")
		return exitUsage
	}
	set := setFlags(sub)
	if len(set) == 0 {
		fmt.Fprintln(stderr, "usage: oswald platforms set api [--host IP] [--port N]")
		return exitUsage
	}
	abs, err := globalRoot(root)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	oldCfg, _ := config.Validate(abs)
	if set["host"] {
		if err := config.SetConfigValue(abs, "default", []string{"platforms", "api", "extra", "api_host"}, *host); err != nil {
			reportPlatformsError(stderr, err)
			return exitError
		}
	}
	if set["port"] {
		if err := config.SetConfigValue(abs, "default", []string{"platforms", "api", "extra", "api_port"}, *port); err != nil {
			reportPlatformsError(stderr, err)
			return exitError
		}
	}
	fmt.Fprintf(stdout, "set platforms.api.extra (%s)\n", effectForChange(abs, oldCfg))
	return exitOK
}
