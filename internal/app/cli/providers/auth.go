package providers

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm/chatgpt"
)

// authCmd dispatches the ChatGPT authorization flows:
//   - auth <name>: single-shot browser OAuth for human CLI use;
//   - auth start <name>: model-led handoff, prints the sign-in URL;
//   - auth finish <name> <code|url>: completes the handoff with the pasted
//     browser response.
func authCmd(t *target, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && strings.TrimSpace(args[0]) == "start" {
		return authStartCmd(t, args[1:], stdout, stderr)
	}
	if len(args) > 0 && strings.TrimSpace(args[0]) == "finish" {
		return authFinishCmd(t, args[1:], stdout, stderr)
	}
	return authLegacyCmd(t, args, stdout, stderr)
}

func authStartCmd(t *target, args []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("providers auth start", flag.ContinueOnError)
	sub.SetOutput(stderr)
	timeoutRaw := sub.String("timeout", "", "handoff validity, e.g. 15m (default 15m)")
	flagArgs, rest := partitionAuthArgs(args)
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		fmt.Fprintln(stderr, authUsage)
		return exitUsage
	}
	name := strings.TrimSpace(rest[0])
	timeout := 15 * time.Minute
	if strings.TrimSpace(*timeoutRaw) != "" {
		parsed, err := time.ParseDuration(strings.TrimSpace(*timeoutRaw))
		if err != nil || parsed <= 0 {
			fmt.Fprintln(stderr, "oswald: invalid --timeout value")
			return exitUsage
		}
		timeout = parsed
	}
	active, endpoint, ok := lookupProvider(t, name, stderr)
	if !ok {
		return exitError
	}
	if endpoint.AuthKind != config.ProviderAuthChatGPT {
		fmt.Fprintf(stderr, "oswald: provider %q is not a ChatGPT subscription provider\n", name)
		return exitError
	}
	ctx := context.Background()
	authURL, expiresAt, err := chatgpt.Start(ctx, active.ProfileRoot, name, timeout)
	if err != nil {
		fmt.Fprintln(stderr, "oswald: "+config.SafeErrorText(err))
		return exitError
	}
	fmt.Fprintf(stdout, "Open this URL to authorize %s:\n%s\n", name, authURL)
	fmt.Fprintf(stdout, "Then run: oswald providers auth finish %s <code|url> (valid until %s)\n", name, expiresAt.UTC().Format(time.RFC3339))
	return exitOK
}

func authFinishCmd(t *target, args []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("providers auth finish", flag.ContinueOnError)
	sub.SetOutput(stderr)
	flagArgs, rest := partitionAuthArgs(args)
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 2 || strings.TrimSpace(rest[0]) == "" || strings.TrimSpace(rest[1]) == "" {
		fmt.Fprintln(stderr, authUsage)
		return exitUsage
	}
	name := strings.TrimSpace(rest[0])
	codeOrURL := rest[1]
	active, endpoint, ok := lookupProvider(t, name, stderr)
	if !ok {
		return exitError
	}
	if endpoint.AuthKind != config.ProviderAuthChatGPT {
		fmt.Fprintf(stderr, "oswald: provider %q is not a ChatGPT subscription provider\n", name)
		return exitError
	}
	ctx := context.Background()
	if err := chatgpt.Finish(ctx, active.ProfileRoot, name, codeOrURL, nil); err != nil {
		fmt.Fprintln(stderr, "oswald: "+config.SafeErrorText(err))
		return exitError
	}
	fmt.Fprintf(stdout, "Authorized %s with ChatGPT subscription\n", name)
	return exitOK
}

func authLegacyCmd(t *target, args []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("providers auth", flag.ContinueOnError)
	sub.SetOutput(stderr)
	noBrowser := sub.Bool("no-browser", false, "print the authorization URL without opening a browser")
	timeoutRaw := sub.String("timeout", "", "authorization wait bound, e.g. 300s")
	flagArgs, rest := partitionAuthArgs(args)
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		fmt.Fprintln(stderr, authUsage)
		return exitUsage
	}
	name := strings.TrimSpace(rest[0])
	active, endpoint, ok := lookupProvider(t, name, stderr)
	if !ok {
		return exitError
	}
	if endpoint.AuthKind != config.ProviderAuthChatGPT {
		fmt.Fprintf(stderr, "oswald: provider %q is not a ChatGPT subscription provider\n", name)
		return exitError
	}
	timeout := 5 * time.Minute
	if strings.TrimSpace(*timeoutRaw) != "" {
		parsed, err := time.ParseDuration(strings.TrimSpace(*timeoutRaw))
		if err != nil || parsed <= 0 {
			fmt.Fprintln(stderr, "oswald: invalid --timeout value")
			return exitUsage
		}
		timeout = parsed
	}
	ctx := context.Background()
	if err := chatgpt.Authorize(ctx, chatgpt.AuthorizeOptions{
		ProfileRoot: active.ProfileRoot, Provider: name, NoBrowser: *noBrowser, Timeout: timeout, Stdin: nil, Stdout: stdout,
	}); err != nil {
		fmt.Fprintln(stderr, "oswald: "+config.SafeErrorText(err))
		return exitError
	}
	return exitOK
}

// lookupProvider resolves the acting profile's effective config and returns
// the named provider endpoint. It reports scope and validation problems.
func lookupProvider(t *target, name string, stderr io.Writer) (*config.Config, config.ProviderEndpoint, bool) {
	cfg, err := config.Validate(t.root)
	if err != nil {
		reportProvidersError(stderr, err)
		return nil, config.ProviderEndpoint{}, false
	}
	active := cfg
	if t.profile != "default" {
		active = cfg.Profiles[t.profile]
		if active == nil {
			fmt.Fprintln(stderr, "oswald: unknown profile")
			return nil, config.ProviderEndpoint{}, false
		}
	}
	endpoint, ok := active.ProviderEndpoints[strings.TrimSpace(name)]
	if !ok {
		fmt.Fprintf(stderr, "oswald: unknown provider %q\n", name)
		return nil, config.ProviderEndpoint{}, false
	}
	return active, endpoint, true
}

func partitionAuthArgs(args []string) (flagArgs, rest []string) {
	valueFlags := map[string]bool{"profile": true, "timeout": true}
	for i := 0; i < len(args); i++ {
		token := args[i]
		name := token
		if j := strings.Index(token, "="); strings.HasPrefix(token, "-") && j >= 0 {
			name = token[:j]
		}
		key := strings.TrimLeft(name, "-")
		if valueFlags[key] {
			flagArgs = append(flagArgs, token)
			if !strings.Contains(token, "=") && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		if key == "no-browser" || key == "n" {
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
