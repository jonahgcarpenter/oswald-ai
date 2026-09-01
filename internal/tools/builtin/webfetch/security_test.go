package webfetch

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
)

type fakeResolver map[string][]netip.Addr

func (r fakeResolver) LookupNetIP(_ context.Context, _ string, host string) ([]netip.Addr, error) {
	return r[host], nil
}

func TestValidateURLCanonicalizesEligiblePublicURLs(t *testing.T) {
	parsed, err := validateURL(" HTTPS://Example.COM.:443/path?q=one#section ")
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.String(); got != "https://example.com/path?q=one" {
		t.Fatalf("canonical URL = %q", got)
	}
	if got := NormalizeURL("https://EXAMPLE.com"); got != "https://example.com/" {
		t.Fatalf("normalized URL = %q", got)
	}
}

func TestValidateURLRejectsIneligibleTargets(t *testing.T) {
	tests := []string{
		"ftp://example.com/file",
		"http://user:password@example.com/",
		"http://localhost/",
		"http://printer.local/",
		"http://service.internal/",
		"http://example.com:8080/",
		"https://example.com:80/",
		"http://127.0.0.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/",
		"http://[::ffff:127.0.0.1]/",
		"https://example.com/?access_token=private",
		"https://example.com/?X-Amz-Signature=private",
		"https://example.com/?sig=private",
		"https://example.com/?X-Goog-Credential=private",
		"https://example.com/?auth=private",
		"https://example.com/\nnext",
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			if _, err := validateURL(raw); err == nil {
				t.Fatalf("validateURL(%q) succeeded", raw)
			}
		})
	}
}

func TestIsPublicIPRejectsSpecialRanges(t *testing.T) {
	private := []string{"0.0.0.1", "10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.1.1", "172.16.0.1", "192.168.0.1", "198.18.0.1", "192.0.2.1", "203.0.113.1", "224.0.0.1", "::1", "64:ff9b::7f00:1", "fc00::1", "fec0::1", "fe80::1", "2001:db8::1"}
	for _, value := range private {
		if isPublicIP(netip.MustParseAddr(value)) {
			t.Fatalf("special address accepted: %s", value)
		}
	}
	for _, value := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !isPublicIP(netip.MustParseAddr(value)) {
			t.Fatalf("public address rejected: %s", value)
		}
	}
}

func TestSecureDialPinsValidatedAddressAndRejectsMixedDNS(t *testing.T) {
	var dialed string
	dial := secureDialContext(fakeResolver{
		"public.example": {netip.MustParseAddr("93.184.216.34")},
	}, func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = address
		return nil, context.Canceled
	})
	_, _ = dial(context.Background(), "tcp", "public.example:443")
	if dialed != "93.184.216.34:443" {
		t.Fatalf("dialed address = %q", dialed)
	}

	called := false
	dial = secureDialContext(fakeResolver{
		"mixed.example": {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("127.0.0.1")},
	}, func(context.Context, string, string) (net.Conn, error) {
		called = true
		return nil, context.Canceled
	})
	if _, err := dial(context.Background(), "tcp", "mixed.example:80"); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("mixed DNS error = %v", err)
	}
	if called {
		t.Fatal("dial was attempted before all DNS answers were validated")
	}
}
