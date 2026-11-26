package providers

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/chatgpt"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/claude"
)

// statusCmd prints every provider's auth state on one screen so the model
// checks once instead of probing per provider.
func statusCmd(t *target, args []string, stdout, stderr io.Writer) int {
	for _, token := range args {
		if strings.TrimSpace(token) != "" {
			fmt.Fprintln(stderr, statusUsage)
			return exitUsage
		}
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
	names := make([]string, 0, len(active.ProviderEndpoints))
	for name := range active.ProviderEndpoints {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Fprintln(stdout, "no providers configured")
		return exitOK
	}
	ctx := context.Background()
	for _, name := range names {
		endpoint := active.ProviderEndpoints[name]
		fmt.Fprintf(stdout, "%s: %s\n", name, describeState(ctx, active, name, endpoint))
	}
	return exitOK
}

func describeState(ctx context.Context, active *config.Config, name string, endpoint config.ProviderEndpoint) string {
	switch endpoint.AuthKind {
	case config.ProviderAuthChatGPT:
		store, err := chatgpt.NewStore(active.ProfileRoot, name)
		if err != nil {
			return "error"
		}
		if pending, ok, _ := store.LoadPending(ctx); ok && !chatgpt.PendingExpired(pending, time.Now()) {
			return "handoff pending (run \"oswald providers auth finish " + name + " <code|url>\")"
		}
		tokens, ok, err := store.Load(ctx)
		if err != nil || !ok || strings.TrimSpace(tokens.AccessToken) == "" {
			return "not authorized (run \"oswald providers auth start " + name + "\")"
		}
		if chatgpt.Expired(tokens, time.Now()) {
			if strings.TrimSpace(tokens.RefreshToken) == "" {
				return "expired (run \"oswald providers auth start " + name + "\")"
			}
			return "authorized (refresh on next use)"
		}
		return "authorized"
	case config.ProviderAuthClaudeSDK:
		if _, err := claude.Check(ctx); err != nil {
			return "not logged in (run \"claude auth login\" on the host, or \"claude setup-token\" headless)"
		}
		return "logged in"
	default:
		if strings.TrimSpace(endpoint.URL) == "" {
			return "no endpoint"
		}
		return "ok"
	}
}
