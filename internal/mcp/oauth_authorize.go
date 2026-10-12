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
	meta, err := discoverOAuthMetadata(ctx, resolved.URL)
	if err != nil {
		return err
	}
	if err := store.SaveMetadata(ctx, *meta); err != nil {
		return err
	}
	flowOpts := FlowOptions{NoBrowser: opts.NoBrowser, Timeout: opts.Timeout, Stdin: opts.Stdin, Stdout: opts.Stdout}
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
