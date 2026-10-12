package mcp

import (
	"context"
	"testing"
)

func TestParseAndValidateURLAcceptsLocalHTTP(t *testing.T) {
	resolver := staticResolver{
		"example.com":       {"93.184.216.34"},
		"private.example":   {"10.0.0.1"},
		"loopback.example":  {"127.0.0.1"},
		"linklocal.example": {"169.254.1.1"},
	}
	for _, rawURL := range []string{
		"https://example.com/mcp",
		"http://example.com/mcp",
		"https://private.example/mcp",
		"http://loopback.example:8080/mcp",
		"https://linklocal.example/mcp",
	} {
		if _, err := parseAndValidateURL(context.Background(), rawURL, resolver); err != nil {
			t.Fatalf("valid URL %s rejected: %v", rawURL, err)
		}
	}
	invalid := []string{
		"https://user:pass@example.com/mcp",
		"ftp://example.com/mcp",
		"https:///mcp",
	}
	for _, rawURL := range invalid {
		if _, err := parseAndValidateURL(context.Background(), rawURL, resolver); err == nil {
			t.Fatalf("expected %s to be rejected", rawURL)
		}
	}
	if _, err := parseAndValidateURL(context.Background(), "https://unknown.example/mcp", resolver); err == nil {
		t.Fatal("expected unresolvable host to be rejected")
	}
}

func TestValidateServerName(t *testing.T) {
	for _, name := range []string{"home", "home_1", "a1"} {
		if err := validateServerName(name); err != nil {
			t.Fatalf("valid name %q rejected: %v", name, err)
		}
	}
	for _, name := range []string{"Ahome", "1home", "h", "home-assistant", "home.assistant", "soul"} {
		if err := validateServerName(name); err == nil {
			t.Fatalf("invalid name %q accepted", name)
		}
	}
}
