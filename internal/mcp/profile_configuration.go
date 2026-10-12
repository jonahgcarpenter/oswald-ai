package mcp

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

type configurationSource interface {
	ListForUser(context.Context, string) ([]ServerConfig, error)
	Get(context.Context, string, string, string) (ServerConfig, bool, error)
}

type profileConfiguration struct {
	owner   string
	servers []ServerConfig
}

// NewProfileManager creates isolated lazy MCP sessions from resolved profile
// configuration. No database, encrypted settings, or account service is used.
// Configuration changes take effect on restart.
func NewProfileManager(owner, profileRoot string, servers []config.MCPServer, log *config.Logger) (*Manager, error) {
	if owner == "" {
		return nil, errors.New("MCP profile owner is required")
	}
	source := &profileConfiguration{owner: owner}
	seen := map[string]bool{}
	for _, entry := range servers {
		if seen[entry.Name] {
			return nil, errors.New("duplicate profile MCP server")
		}
		seen[entry.Name] = true
		if err := validateServerName(entry.Name); err != nil {
			return nil, err
		}
		resolved, err := resolveProfileServer(entry)
		if err != nil {
			return nil, err
		}
		resolved.ID = rand.Text()
		resolved.Scope = ScopeUser
		resolved.OwnerUserID = owner
		source.servers = append(source.servers, resolved)
	}
	manager := newManager(source, log)
	manager.profileRoot = profileRoot
	manager.log.Debug("mcp.profile.configured", "configured profile MCP servers", config.F("profile", owner), config.F("server_count", len(servers)))
	return manager, nil
}

// resolveProfileServer validates one operator-owned server entry and copies
// its credentials so later caller mutation cannot affect resolved state.
// Exactly one of URL (HTTP) or Command (stdio) must be set; configuration
// loading enforces the same contract, and this is defense in depth.
func resolveProfileServer(entry config.MCPServer) (ServerConfig, error) {
	hasURL := strings.TrimSpace(entry.URL) != ""
	hasCommand := strings.TrimSpace(entry.Command) != ""
	if hasURL == hasCommand {
		return ServerConfig{}, errors.New("profile MCP server requires exactly one of url or command")
	}
	if hasCommand {
		if strings.TrimSpace(entry.Transport) != "" && entry.Transport != TransportStdio {
			return ServerConfig{}, errors.New("profile MCP stdio server does not use transport")
		}
		if len(entry.Headers) > 0 {
			return ServerConfig{}, errors.New("profile MCP stdio server does not use headers")
		}
		if strings.TrimSpace(entry.Auth) != "" {
			return ServerConfig{}, errors.New("profile MCP stdio server does not use OAuth")
		}
		if strings.TrimSpace(entry.Command) == "" {
			return ServerConfig{}, errors.New("invalid profile MCP command")
		}
		env := make(map[string]string, len(entry.Env))
		for key, value := range entry.Env {
			env[key] = value
		}
		return ServerConfig{
			Name: entry.Name, Transport: TransportStdio,
			Command: strings.TrimSpace(entry.Command), Args: append([]string(nil), entry.Args...),
			Cwd: strings.TrimSpace(entry.Cwd), Env: env,
			Include: append([]string(nil), entry.Include...), Exclude: append([]string(nil), entry.Exclude...),
			Enabled: entry.Enabled,
		}, nil
	}
	endpoint, err := url.Parse(entry.URL)
	if err != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.Host == "" || endpoint.User != nil {
		return ServerConfig{}, errors.New("invalid profile MCP endpoint")
	}
	if len(entry.Args) > 0 || strings.TrimSpace(entry.Cwd) != "" || len(entry.Env) > 0 {
		return ServerConfig{}, errors.New("profile MCP HTTP server does not use stdio fields")
	}
	transport := entry.Transport
	if transport == "" {
		transport = TransportStreamableHTTP
	}
	if transport != TransportStreamableHTTP && transport != TransportSSE {
		return ServerConfig{}, errors.New("unsupported profile MCP transport")
	}
	headers := make(map[string]string, len(entry.Headers))
	for key, value := range entry.Headers {
		headers[key] = value
	}
	resolved := ServerConfig{
		Name: entry.Name, Transport: transport, URL: entry.URL, Headers: headers,
		Include: append([]string(nil), entry.Include...), Exclude: append([]string(nil), entry.Exclude...),
		Enabled: entry.Enabled,
	}
	if strings.TrimSpace(entry.Auth) != "" {
		if entry.Auth != "oauth" || transport != TransportStreamableHTTP {
			return ServerConfig{}, errors.New("profile MCP OAuth requires a Streamable HTTP url")
		}
		for key := range entry.Headers {
			if strings.EqualFold(key, "Authorization") {
				return ServerConfig{}, errors.New("profile MCP OAuth owns the Authorization header")
			}
		}
		flow := entry.OAuth.Flow
		if flow == "" {
			flow = "browser"
		}
		if flow != "browser" && flow != "device" {
			return ServerConfig{}, errors.New("unsupported profile MCP OAuth flow")
		}
		timeout := entry.OAuth.Timeout
		if timeout <= 0 {
			timeout = 5 * time.Minute
		}
		resolved.OAuth = &OAuthConfig{
			ClientID: entry.OAuth.ClientID, ClientSecret: entry.OAuth.ClientSecret,
			Flow: flow, Scopes: append([]string(nil), entry.OAuth.Scopes...), Timeout: timeout,
		}
	}
	return resolved, nil
}

func (s *profileConfiguration) ListForUser(ctx context.Context, owner string) ([]ServerConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if owner != s.owner {
		return nil, nil
	}
	return append([]ServerConfig(nil), s.servers...), nil
}

func (s *profileConfiguration) Get(ctx context.Context, scope, owner, name string) (ServerConfig, bool, error) {
	if err := ctx.Err(); err != nil {
		return ServerConfig{}, false, err
	}
	if scope != ScopeUser || owner != s.owner {
		return ServerConfig{}, false, nil
	}
	for _, entry := range s.servers {
		if entry.Name == name {
			return entry, true, nil
		}
	}
	return ServerConfig{}, false, nil
}
