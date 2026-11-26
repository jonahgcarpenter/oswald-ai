package providers

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/chatgpt"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/claude"
)

func testCmd(t *target, args []string, stdout, stderr io.Writer) int {
	var name string
	for _, token := range args {
		if strings.HasPrefix(token, "-") {
			continue
		}
		name = token
	}
	name = strings.TrimSpace(name)
	if name == "" {
		fmt.Fprintln(stderr, testUsage)
		return exitUsage
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	switch endpoint.AuthKind {
	case config.ProviderAuthChatGPT:
		store, err := chatgpt.NewStore(active.ProfileRoot, name)
		if err != nil {
			fmt.Fprintln(stderr, "oswald: "+config.SafeErrorText(err))
			return exitError
		}
		tokens, ok, err := store.Load(ctx)
		if err != nil || !ok || strings.TrimSpace(tokens.AccessToken) == "" {
			fmt.Fprintf(stdout, "%s: not authorized (run \"oswald providers auth %s\")\n", name, name)
			return exitOK
		}
		if chatgpt.Expired(tokens, time.Now()) && strings.TrimSpace(tokens.RefreshToken) == "" {
			fmt.Fprintf(stdout, "%s: expired (run \"oswald providers auth %s\")\n", name, name)
			return exitOK
		}
		fmt.Fprintf(stdout, "%s: authorized\n", name)
		return exitOK
	case config.ProviderAuthClaudeSDK:
		if _, err := claude.Check(ctx); err != nil {
			fmt.Fprintf(stdout, "%s: not logged in (run \"claude auth login\")\n", name)
			return exitOK
		}
		fmt.Fprintf(stdout, "%s: logged in\n", name)
		return exitOK
	default:
		if strings.TrimSpace(endpoint.URL) == "" {
			fmt.Fprintf(stdout, "%s: no endpoint\n", name)
			return exitOK
		}
		fmt.Fprintf(stdout, "%s: ok\n", name)
		return exitOK
	}
}
