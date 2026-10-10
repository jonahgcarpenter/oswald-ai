package mcp

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"

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
func NewProfileManager(owner string, servers []config.MCPServer, log *config.Logger) (*Manager, error) {
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
		if _, err := normalizeServerDescription(entry.Description); err != nil {
			return nil, err
		}
		endpoint, err := url.Parse(entry.URL)
		if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil {
			return nil, errors.New("invalid profile MCP endpoint")
		}
		if entry.Transport == "" {
			entry.Transport = TransportStreamableHTTP
		}
		if entry.Transport != TransportStreamableHTTP {
			return nil, errors.New("unsupported profile MCP transport")
		}
		headers := make(map[string]string, len(entry.Headers))
		for key, value := range entry.Headers {
			headers[key] = value
		}
		source.servers = append(source.servers, ServerConfig{ID: rand.Text(), Scope: ScopeUser, OwnerUserID: owner, Name: entry.Name, Description: entry.Description, Transport: entry.Transport, URL: entry.URL, Headers: headers, Enabled: entry.Enabled})
	}
	manager := newManager(source, log)
	manager.log.Debug("mcp.profile.configured", "configured profile MCP servers", config.F("profile", owner), config.F("server_count", len(servers)), config.F("status", "ok"))
	return manager, nil
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
