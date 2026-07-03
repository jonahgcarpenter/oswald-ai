package mcp

import (
	"context"
	"testing"
)

func TestParseAndValidateURLRequiresHTTPSPublicAddress(t *testing.T) {
	resolver := staticResolver{
		"example.com":       {"93.184.216.34"},
		"private.example":   {"10.0.0.1"},
		"loopback.example":  {"127.0.0.1"},
		"linklocal.example": {"169.254.1.1"},
	}
	if _, err := parseAndValidateURL(context.Background(), "https://example.com/mcp", resolver); err != nil {
		t.Fatalf("valid URL rejected: %v", err)
	}
	invalid := []string{
		"http://example.com/mcp",
		"https://user:pass@example.com/mcp",
		"https://private.example/mcp",
		"https://loopback.example/mcp",
		"https://linklocal.example/mcp",
	}
	for _, rawURL := range invalid {
		if _, err := parseAndValidateURL(context.Background(), rawURL, resolver); err == nil {
			t.Fatalf("expected %s to be rejected", rawURL)
		}
	}
}

func TestValidateServerName(t *testing.T) {
	for _, name := range []string{"home", "home_1", "a1"} {
		if err := validateServerName(name); err != nil {
			t.Fatalf("valid name %q rejected: %v", name, err)
		}
	}
	for _, name := range []string{"Ahome", "1home", "h", "home-assistant", "home.assistant"} {
		if err := validateServerName(name); err == nil {
			t.Fatalf("invalid name %q accepted", name)
		}
	}
}
