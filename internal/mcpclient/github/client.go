package githubmcp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	DefaultURL      = "https://api.githubcopilot.com/mcp/"
	DefaultToolsets = "context,repos"
)

type Client struct {
	session *gomcp.ClientSession
}

func Connect(ctx context.Context, cfg *config.Config, log *config.Logger) (*Client, error) {
	endpoint := DefaultURL
	toolsets := DefaultToolsets

	log.Server("mcp.github").Info("mcp.server.connect.start", "connecting MCP server", config.F("server", "github"), config.F("transport", "streamable_http"), config.F("url", endpoint), config.F("toolsets", toolsets))

	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &transport{
			base:     http.DefaultTransport,
			token:    cfg.GitHubMCPToken,
			toolsets: toolsets,
		},
	}
	client := gomcp.NewClient(&gomcp.Implementation{Name: "oswald-ai", Version: "1.0.0"}, &gomcp.ClientOptions{Capabilities: &gomcp.ClientCapabilities{}})
	streamableTransport := &gomcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}

	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	session, err := client.Connect(connectCtx, streamableTransport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect session: %w", err)
	}

	return &Client{session: session}, nil
}

func (c *Client) Session() *gomcp.ClientSession {
	if c == nil {
		return nil
	}
	return c.session
}

func (c *Client) Close() error {
	if c == nil || c.session == nil {
		return nil
	}
	return c.session.Close()
}

type transport struct {
	base     http.RoundTripper
	token    string
	toolsets string
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	clone.Header.Set("X-MCP-Readonly", "true")
	if strings.TrimSpace(t.toolsets) != "" {
		clone.Header.Set("X-MCP-Toolsets", t.toolsets)
	}
	return base.RoundTrip(clone)
}
