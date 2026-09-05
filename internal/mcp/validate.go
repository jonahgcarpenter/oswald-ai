package mcp

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxProviderIdentifierBytes    = 64
	maxServerDescriptionRuneCount = 500
	maxToolDescriptionRuneCount   = 1000
	maxParamDescriptionRuneCount  = 500
	maxEnumValueCount             = 100
	maxEnumValueRuneCount         = 200
	maxEnumTotalRuneCount         = 4000
)

func validateProviderIdentifier(name string) error {
	if len(name) == 0 || len(name) > maxProviderIdentifierBytes {
		return fmt.Errorf("identifier must contain 1-%d bytes", maxProviderIdentifierBytes)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if i == 0 {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '_' {
				return fmt.Errorf("identifier must start with an ASCII letter or underscore")
			}
			continue
		}
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return fmt.Errorf("identifier may contain only ASCII letters, numbers, underscores, and hyphens")
		}
	}
	return nil
}

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

func normalizeServerDescription(description string) (string, error) {
	description = strings.TrimSpace(description)
	if description == "" {
		return "", fmt.Errorf("MCP server description cannot be empty")
	}
	if utf8.RuneCountInString(description) > maxServerDescriptionRuneCount {
		return "", fmt.Errorf("MCP server description must be at most %d characters", maxServerDescriptionRuneCount)
	}
	for _, r := range description {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("MCP server description cannot contain control characters")
		}
	}
	return description, nil
}

func normalizeCatalogDescription(description string, maxRunes int) string {
	description = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return ' '
		}
		return r
	}, description)
	description = strings.Join(strings.Fields(description), " ")
	runes := []rune(description)
	if len(runes) > maxRunes {
		description = strings.TrimSpace(string(runes[:maxRunes]))
	}
	return description
}

func safeCatalogEnum(values []string) []string {
	if len(values) == 0 || len(values) > maxEnumValueCount {
		return nil
	}
	totalRunes := 0
	for _, value := range values {
		runeCount := utf8.RuneCountInString(value)
		if runeCount > maxEnumValueRuneCount {
			return nil
		}
		totalRunes += runeCount
		if totalRunes > maxEnumTotalRuneCount {
			return nil
		}
		for _, r := range value {
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
				return nil
			}
		}
	}
	return append([]string(nil), values...)
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
