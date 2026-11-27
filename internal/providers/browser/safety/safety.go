package browser

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/vault"
)

// maxURLBytes bounds navigated URLs.
const maxURLBytes = 2048

// secretParamName reports whether a URL query parameter name carries key
// material: api keys, secrets (covering client_secret), and passwords.
// Generic login-flow names (token, auth, code, state, session_id) are
// deliberately allowed so OAuth callbacks, magic links, and invite URLs work.
func secretParamName(name string) bool {
	lower := strings.ToLower(name)
	if strings.Contains(lower, "secret") {
		return true
	}
	compact := strings.NewReplacer("_", "", "-", "", ".", "").Replace(lower)
	switch compact {
	case "apikey", "password", "passwd":
		return true
	}
	return false
}

// hasSecretParam reports whether the raw query string carries a valued
// credential-named parameter. Percent-encoded forms are checked too. Only
// parameter names are inspected, never the path or free text.
func hasSecretParam(rawQuery string) bool {
	check := func(query string) bool {
		values, _ := url.ParseQuery(query)
		for name, vals := range values {
			if !secretParamName(name) {
				continue
			}
			for _, value := range vals {
				if strings.TrimSpace(value) != "" {
					return true
				}
			}
		}
		return false
	}
	if check(rawQuery) {
		return true
	}
	if decoded, err := url.QueryUnescape(rawQuery); err == nil && decoded != rawQuery {
		return check(decoded)
	}
	return false
}

var cloudMetadataHosts = map[string]bool{
	"169.254.169.254":          true,
	"metadata.google.internal": true,
	"metadata.google":          true,
	"metadata.azure.internal":  true,
	"100.100.100.200":          true,
	"192.0.0.192":              true,
}

// sensitiveEvalPatterns gates risky primitives by name: cookie and storage
// reads, network emission, and form-value extraction. It blocks names, not
// actual exfiltration, so legitimate DOM reads can trip it; it stays behind
// the restrict_evaluate opt-in while private-destination egress stays gated
// separately in Eval regardless of the flag.
var sensitiveEvalPatterns = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`(?i)\bdocument\s*\.\s*cookie\b`), "document.cookie"},
	{regexp.MustCompile(`(?i)\b(?:localStorage|sessionStorage)\b`), "web storage"},
	{regexp.MustCompile(`(?i)\bindexedDB\b`), "IndexedDB"},
	{regexp.MustCompile(`(?i)\bcaches\s*\.\s*(?:open|match|keys)\b`), "Cache Storage"},
	{regexp.MustCompile(`(?i)\bnavigator\s*\.\s*(?:clipboard|credentials|serviceWorker)\b`), "navigator sensitive API"},
	{regexp.MustCompile(`(?i)\b(?:fetch|XMLHttpRequest|WebSocket|EventSource)\s*\(`), "network request"},
	{regexp.MustCompile(`(?i)\bnavigator\s*\.\s*sendBeacon\s*\(`), "network beacon"},
	{regexp.MustCompile(`(?is)\bdocument\s*\.\s*forms\b.*\bvalue\b`), "form value extraction"},
	{regexp.MustCompile(`(?is)\bquerySelector(?:All)?\s*\([^)]*(?:input|textarea|password)[^)]*\).*\bvalue\b`), "form value extraction"},
}

// sensitiveEvalTokens catches quoted spellings (document["cookie"],
// globalThis["fetch"]) by matching string literals as well as direct
// spellings.
var sensitiveEvalTokens = []struct {
	token  string
	reason string
}{
	{"cookie", "document.cookie"},
	{"localstorage", "web storage"},
	{"sessionstorage", "web storage"},
	{"indexeddb", "IndexedDB"},
	{"caches", "Cache Storage"},
	{"clipboard", "navigator sensitive API"},
	{"credentials", "navigator sensitive API"},
	{"serviceworker", "navigator sensitive API"},
	{"fetch", "network request"},
	{"xmlhttprequest", "network request"},
	{"websocket", "network request"},
	{"eventsource", "network request"},
	{"sendbeacon", "network beacon"},
}

var jsStringLiteralRE = regexp.MustCompile(`'(?:\\.|[^'\\])*'|"(?:\\.|[^"\\])*"|` + "`(?:\\.|[^`\\\\])*`")

// sensitiveEvalReason returns why an expression trips the sensitive-primitive
// denylist, or empty when it does not.
func sensitiveEvalReason(expr string) string {
	if expr == "" {
		return ""
	}
	for _, p := range sensitiveEvalPatterns {
		if p.re.MatchString(expr) {
			return p.reason
		}
	}
	lowered := strings.ToLower(expr)
	var literals strings.Builder
	for _, m := range jsStringLiteralRE.FindAllString(expr, -1) {
		literals.WriteString(strings.ToLower(m))
	}
	lit := literals.String()
	for _, t := range sensitiveEvalTokens {
		if strings.Contains(lowered, t.token) || strings.Contains(lit, t.token) {
			return t.reason
		}
	}
	return ""
}

// RedactSecrets replaces fixed-shape credential material. It is pattern-based,
// not a secret detector; callers must still avoid logging page content.
func RedactSecrets(s string) string {
	out := s
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9\-._~+/=]{16,}`),
		regexp.MustCompile(`(?i)\b(api[_-]?key|secret|token|password)\b\s*[:=]\s*['"]?([A-Za-z0-9\-._~+/=]{12,})['"]?`),
		regexp.MustCompile(`\bsk-[A-Za-z0-9]{16,}\b`),
		regexp.MustCompile(`\bghp_[A-Za-z0-9]{16,}\b`),
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{16,}\b`),
		regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),
	} {
		out = re.ReplaceAllString(out, "[REDACTED]")
	}
	// Vault-resolved secrets are exact values, not shapes: scrub them so a
	// page echo cannot leak a filled password back to the model.
	return vault.ScrubRegistered(out)
}

// ValidateNavigateURL parses and enforces the navigation safety pipeline on
// the raw model-supplied URL: secret-URL refusal, metadata block, scheme and
// private-destination checks. It returns the canonical URL.
func ValidateNavigateURL(raw string, allowPrivate, allowInsecure bool) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > maxURLBytes {
		return "", fmt.Errorf("invalid URL")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("invalid URL")
	}
	if hasSecretParam(parsed.RawQuery) {
		return "", fmt.Errorf("URL is blocked")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("unsupported URL scheme")
	}
	if scheme == "http" && !allowInsecure {
		return "", fmt.Errorf("http URLs are blocked (set tools.browser.allow_insecure_urls to allow)")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" {
		return "", fmt.Errorf("invalid URL")
	}
	if cloudMetadataHosts[host] || cloudMetadataHosts[strings.TrimSuffix(parsed.Host, ".")] {
		return "", fmt.Errorf("URL is blocked")
	}
	if !allowPrivate {
		if blocked, reason := isPrivateDestination(host); blocked {
			return "", fmt.Errorf("URL is blocked (%s)", reason)
		}
	}
	parsed.Fragment = ""
	canonical := parsed.String()
	if len(canonical) > maxURLBytes {
		return "", fmt.Errorf("invalid URL")
	}
	return canonical, nil
}

// CheckFinalURL revalidates the post-navigation URL. Callers must move the
// page to about:blank before returning a blocked error so later snapshots
// cannot read internal content. Each failure names its reason plus the final
// page's scheme/host so the model can tell a redirect, an error page, and a
// policy block apart; query values are never included.
func CheckFinalURL(raw string, allowPrivate, allowInsecure bool) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "about:blank" {
		return nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return fmt.Errorf("navigation landed on an invalid URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "https":
	case "http":
		if !allowInsecure {
			return fmt.Errorf("navigation landed on an http URL (set tools.browser.allow_insecure_urls to allow)")
		}
	default:
		return fmt.Errorf("navigation landed on an unsupported page (scheme %s)", shortToken(scheme))
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if cloudMetadataHosts[host] {
		return fmt.Errorf("navigation landed on a blocked metadata destination (%s)", host)
	}
	if !allowPrivate {
		if blocked, reason := isPrivateDestination(host); blocked {
			return fmt.Errorf("navigation landed on a blocked %s destination (%s)", reason, host)
		}
	}
	if hasSecretParam(parsed.RawQuery) {
		return fmt.Errorf("navigation landed on a URL with a secret parameter")
	}
	return nil
}

// shortToken caps an echoed URL token (scheme/host) so a malformed page
// address cannot bloat the model-visible error.
func shortToken(value string) string {
	if len(value) > 64 {
		return value[:64] + "..."
	}
	if value == "" {
		return "(empty)"
	}
	return value
}

// isPrivateDestination reports loopback/private/link-local/multicast and CGNAT
// destinations. DNS failure is not private (fail open only when the page-URL
// probe itself fails, per the read/input guards).
func isPrivateDestination(host string) (bool, string) {
	if strings.EqualFold(host, "localhost") {
		return true, "loopback"
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() {
			return true, "private"
		}
		if addr.Is4() && isCGNAT(addr) {
			return true, "private"
		}
		return false, ""
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return false, ""
	}
	for _, ip := range ips {
		addr, err := netip.ParseAddr(ip.String())
		if err != nil {
			continue
		}
		if addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() {
			return true, "private"
		}
		if addr.Is4() && isCGNAT(addr) {
			return true, "private"
		}
	}
	return false, ""
}

func isCGNAT(addr netip.Addr) bool {
	if !addr.Is4() {
		return false
	}
	b := addr.As4()
	return b[0] == 100 && b[1] >= 64 && b[1] <= 127
}
