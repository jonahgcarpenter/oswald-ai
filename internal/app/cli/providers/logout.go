package providers

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm/chatgpt"
)

// logoutCmd forgets a ChatGPT subscription login (tokens plus any in-flight
// handoff) so a dead grant resets cleanly. Claude logins stay owned by the
// `claude` CLI; key providers have nothing to forget.
func logoutCmd(t *target, args []string, stdout, stderr io.Writer) int {
	var name string
	for _, token := range args {
		if strings.HasPrefix(token, "-") {
			continue
		}
		name = token
	}
	name = strings.TrimSpace(name)
	if name == "" {
		fmt.Fprintln(stderr, logoutUsage)
		return exitUsage
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
	store, err := chatgpt.NewStore(active.ProfileRoot, name)
	if err != nil {
		fmt.Fprintln(stderr, "oswald: "+config.SafeErrorText(err))
		return exitError
	}
	if err := store.Clear(ctx); err != nil {
		fmt.Fprintln(stderr, "oswald: "+config.SafeErrorText(err))
		return exitError
	}
	_ = store.ClearPending(ctx)
	fmt.Fprintf(stdout, "provider %s logged out\n", name)
	return exitOK
}
