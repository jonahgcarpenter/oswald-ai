package webfetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxURLRunes = 2048

type resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

var nonPublicPrefixes = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64",
	"2001:db8::/32", "2002::/16", "fc00::/7", "fec0::/10", "fe80::/10", "ff00::/8",
)

func mustPrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefixes = append(prefixes, netip.MustParsePrefix(value))
	}
	return prefixes
}

func validateURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !utf8.ValidString(raw) || utf8.RuneCountInString(raw) > maxURLRunes {
		return nil, errors.New("URL must be valid UTF-8 between 1 and 2048 characters")
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return nil, errors.New("URL must not contain control characters")
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("URL is invalid")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("URL must use http or https")
	}
	if parsed.User != nil {
		return nil, errors.New("URL must not include user information")
	}
	if parsed.Opaque != "" {
		return nil, errors.New("URL must use hierarchical HTTP syntax")
	}
	port := parsed.Port()
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if hostname == "" {
		return nil, errors.New("URL must include a host")
	}
	if err := validateHostname(hostname); err != nil {
		return nil, err
	}
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || (parsed.Scheme == "http" && portNumber != 80) || (parsed.Scheme == "https" && portNumber != 443) {
			return nil, errors.New("URL must use the standard port for its scheme")
		}
	}
	parsed.Host = hostname
	if strings.Contains(hostname, ":") {
		parsed.Host = "[" + hostname + "]"
	}
	if hasSecretQueryParameter(parsed.Query()) {
		return nil, errors.New("URL query parameters appear to contain authentication data")
	}
	parsed.Fragment = ""
	parsed.RawFragment = ""
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return parsed, nil
}

// NormalizeURL returns a stable representation for request-local duplicate detection.
// Invalid values remain distinguishable and are rejected by the handler.
func NormalizeURL(raw string) string {
	parsed, err := validateURL(raw)
	if err != nil {
		return strings.TrimSpace(raw)
	}
	return parsed.String()
}

func validateHostname(hostname string) error {
	if strings.Contains(hostname, "%") {
		return errors.New("URL host must not include an IPv6 zone")
	}
	if ip, err := netip.ParseAddr(hostname); err == nil {
		if !isPublicIP(ip) {
			return errors.New("URL host is not publicly routable")
		}
		return nil
	}
	if !strings.Contains(hostname, ".") || hostname == "localhost" ||
		strings.HasSuffix(hostname, ".localhost") || strings.HasSuffix(hostname, ".local") ||
		strings.HasSuffix(hostname, ".internal") || strings.HasSuffix(hostname, ".home.arpa") ||
		strings.HasSuffix(hostname, ".onion") {
		return errors.New("URL host is not a public hostname")
	}
	return nil
}

func hasSecretQueryParameter(values url.Values) bool {
	for key := range values {
		normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(key))
		switch normalized {
		case "apikey", "accesskey", "accesstoken", "auth", "authkey", "authtoken", "authorization",
			"bearer", "clientsecret", "code", "credential", "jwt", "key", "password", "passwd",
			"secret", "sessionid", "sig", "signature", "signed", "token", "xamzcredential",
			"xamzsecuritytoken", "xamzsignature", "xgoogcredential", "xgoogsignature":
			return true
		}
		if strings.HasSuffix(normalized, "apikey") || strings.HasSuffix(normalized, "credential") ||
			strings.HasSuffix(normalized, "password") || strings.HasSuffix(normalized, "secret") ||
			strings.HasSuffix(normalized, "signature") || strings.HasSuffix(normalized, "token") {
			return true
		}
	}
	return false
}

func isPublicIP(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func secureDialContext(resolve resolver, dial dialContextFunc) dialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("invalid fetch destination")
		}
		var addrs []netip.Addr
		if literal, parseErr := netip.ParseAddr(host); parseErr == nil {
			addrs = []netip.Addr{literal}
		} else {
			lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			addrs, err = resolve.LookupNetIP(lookupCtx, "ip", host)
			if err != nil || len(addrs) == 0 {
				return nil, errors.New("fetch host could not be resolved")
			}
		}
		for _, addr := range addrs {
			if !isPublicIP(addr) {
				return nil, errors.New("fetch host resolved to a non-public address")
			}
		}
		var lastErr error
		for _, addr := range addrs {
			conn, dialErr := dial(ctx, network, net.JoinHostPort(addr.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		return nil, fmt.Errorf("connect to fetch host: %w", lastErr)
	}
}
