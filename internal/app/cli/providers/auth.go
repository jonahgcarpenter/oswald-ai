package providers

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/chatgpt"
)

func authCmd(t *target, args []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("providers auth", flag.ContinueOnError)
	sub.SetOutput(stderr)
	noBrowser := sub.Bool("no-browser", false, "print the authorization URL without opening a browser")
	timeoutRaw := sub.String("timeout", "", "authorization wait bound, e.g. 300s")
	profileFlag := sub.String("profile", "", "profile name")
	flagArgs, rest := partitionAuthArgs(args, *profileFlag)
	_ = profileFlag
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		fmt.Fprintln(stderr, authUsage)
		return exitUsage
	}
	name := strings.TrimSpace(rest[0])
	cfg, err := config.Validate(t.root)
	if err != nil {
		reportProvidersError(stderr, err)
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
	endpoint, ok := active.ProviderEndpoints[name]
	if !ok {
		fmt.Fprintf(stderr, "oswald: unknown provider %q\n", name)
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
	profileRoot := active.ProfileRoot
	if strings.TrimSpace(profileRoot) == "" {
		fmt.Fprintln(stderr, "oswald: profile has no root")
		return exitError
	}
	ctx := context.Background()
	if err := chatgpt.Authorize(ctx, chatgpt.AuthorizeOptions{
		ProfileRoot: profileRoot, Provider: name, NoBrowser: *noBrowser, Timeout: timeout, Stdin: nil, Stdout: stdout,
	}); err != nil {
		if ctx.Err() != nil {
			return exitError
		}
		fmt.Fprintln(stderr, "oswald: "+config.SafeErrorText(err))
		return exitError
	}
	return exitOK
}

func partitionAuthArgs(args []string, _ string) (flagArgs, rest []string) {
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
