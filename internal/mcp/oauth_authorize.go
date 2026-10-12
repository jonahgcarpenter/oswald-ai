package mcp

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// AuthorizeOptions drives one interactive OAuth authorization run.
type AuthorizeOptions struct {
	ProfileRoot string
	Server      config.MCPServer
	Flow        string
	NoBrowser   bool
	Timeout     time.Duration
	Stdin       io.Reader
	Stdout      io.Writer
}

// AuthorizeServer runs discovery plus the configured interactive flow for one
// profile MCP server and persists the credential. It is called by the
// `oswald mcp auth` command; the service never calls it.
func AuthorizeServer(ctx context.Context, opts AuthorizeOptions) error {
	if strings.TrimSpace(opts.ProfileRoot) == "" {
		return fmt.Errorf("profile root is required")
	}
	resolved, err := resolveProfileServer(opts.Server)
	if err != nil {
		return err
	}
	if resolved.OAuth == nil {
		return fmt.Errorf("server %q is not configured for OAuth", opts.Server.Name)
	}
	oauth := *resolved.OAuth
	if flow := strings.TrimSpace(opts.Flow); flow != "" {
		if flow != "browser" && flow != "device" {
			return fmt.Errorf("unsupported OAuth flow %q", flow)
		}
		oauth.Flow = flow
	}
	store, err := newTokenStore(opts.ProfileRoot, resolved.Name)
	if err != nil {
		return err
	}
	httpClient := discoveryClient()
	// Harvest a live challenge first (best-effort): a 401/403 scope
	// parameter outranks advertised scopes, and a resource_metadata URL is
	// preferred as the first discovery candidate. Harvest failures fall
	// through to plain discovery and advertised/omitted scopes.
	challenge, harvestedResourceURL := fetchChallenge(ctx, httpClient, resolved.URL)
	meta, err := discoverOAuthMetadataWithChallenge(ctx, resolved.URL, httpClient, harvestedResourceURL)
	if err != nil {
		return err
	}
	if err := store.SaveMetadata(ctx, *meta); err != nil {
		return err
	}
	// A changed operator client identity wedges refresh with invalid_client
	// until manual wipe; drop stale tokens so this run starts clean.
	if id := strings.TrimSpace(oauth.ClientID); id != "" {
		if stored, ok, err := store.LoadTokens(ctx); err != nil {
			return err
		} else if ok && stored.ClientID != "" && stored.ClientID != id {
			if err := store.ClearTokens(ctx); err != nil {
				return err
			}
		}
	}
	// Automatic scope selection: explicit config wins, else the live
	// challenge (unioned with the stored grant for step-up), else
	// advertised PRM/AS scopes, else omitted so the server grants its
	// default set. The local copy never writes back to YAML. Only the
	// selection source and count are reported, never scope values.
	source := scopeSelectionSource(oauth.Scopes, challenge, meta)
	var storedScopes []string
	if stored, ok, err := store.LoadTokens(ctx); err == nil && ok {
		storedScopes = stored.Scopes
	}
	oauth.Scopes = cliEffectiveScopes(oauth.Scopes, storedScopes, challenge, meta)
	if opts.Stdout != nil {
		fmt.Fprintf(opts.Stdout, "OAuth scope selection: %s (%d scope(s)).\n", source, len(oauth.Scopes))
	}
	flowOpts := FlowOptions{NoBrowser: opts.NoBrowser, Timeout: opts.Timeout, Stdin: opts.Stdin, Stdout: opts.Stdout, ChallengeScope: challenge}
	var tokens *TokenSet
	switch oauth.Flow {
	case "device":
		tokens, err = RunDeviceFlow(ctx, store, &oauth, meta, resolved.URL, flowOpts)
	default:
		tokens, err = RunBrowserFlow(ctx, store, &oauth, meta, resolved.URL, flowOpts)
	}
	if err != nil {
		return err
	}
	if err := store.SaveTokens(ctx, *tokens); err != nil {
		return err
	}
	host := tokens.Issuer
	if parsed, err := url.Parse(tokens.Issuer); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	expiry := "no expiry"
	if !tokens.Expiry.IsZero() {
		expiry = "expires " + tokens.Expiry.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(opts.Stdout, "Authorized %s with %s (%d scope(s), %s).\n", resolved.Name, host, len(tokens.Scopes), expiry)
	return nil
}
