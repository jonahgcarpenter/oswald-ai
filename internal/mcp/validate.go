package mcp

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

type hostnameResolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

func validateServerName(name string) error {
	if len(name) < 2 || len(name) > 40 {
		return fmt.Errorf("server name must be 2-40 characters")
	}
	for i, r := range name {
		if i == 0 {
			if r < 'a' || r > 'z' {
				return fmt.Errorf("server name must start with a lowercase letter")
			}
			continue
		}
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return fmt.Errorf("server name may contain only lowercase letters, numbers, and underscores")
		}
	}
	if isReservedServerName(name) {
		return fmt.Errorf("server name %q is reserved", name)
	}
	return nil
}

func isReservedServerName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), "soul")
}

func isReservedToolName(name string) bool {
	server, _, ok := splitToolName(name)
	return ok && isReservedServerName(server)
}

func validateScope(scope, ownerUserID string) error {
	switch scope {
	case ScopeGlobal:
		if strings.TrimSpace(ownerUserID) != "" {
			return fmt.Errorf("global MCP servers cannot have an owner user")
		}
	case ScopeUser:
		if strings.TrimSpace(ownerUserID) == "" {
			return fmt.Errorf("user MCP servers require an owner user")
		}
	default:
		return fmt.Errorf("unsupported MCP server scope %q", scope)
	}
	return nil
}

func validateTransport(transport string) error {
	switch strings.TrimSpace(transport) {
	case TransportStreamableHTTP, TransportSSE:
		return nil
	default:
		return fmt.Errorf("unsupported MCP transport %q", transport)
	}
}

func parseAndValidateURL(ctx context.Context, rawURL string, resolver hostnameResolver) (*url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse MCP server URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("MCP server URL must use https")
	}
	if parsed.Hostname() == "" {
		return nil, fmt.Errorf("MCP server URL must include a host")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("MCP server URL must not include user info")
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := resolver.LookupHost(lookupCtx, parsed.Hostname())
	if err != nil {
		return nil, fmt.Errorf("resolve MCP server host: %w", err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve MCP server host: no addresses")
	}
	for _, addr := range addrs {
		ip, err := netip.ParseAddr(addr)
		if err != nil {
			return nil, fmt.Errorf("parse resolved MCP server address: %w", err)
		}
		if !isPublicRoutable(ip) {
			return nil, fmt.Errorf("MCP server host resolves to non-public address")
		}
	}
	return parsed, nil
}

func isPublicRoutable(ip netip.Addr) bool {
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	return true
}
