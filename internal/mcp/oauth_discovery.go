package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

const maxMetadataBytes = 64 * 1024

// serverMetadata is the discovered OAuth configuration for one MCP server.
// It is cached in <server>.meta.json.
type serverMetadata struct {
	// Resource is request-local discovery context, not a persisted field.
	Resource                    string   `json:"-"`
	Issuer                      string   `json:"issuer"`
	AuthorizationEndpoint       string   `json:"authorization_endpoint"`
	TokenEndpoint               string   `json:"token_endpoint"`
	DeviceAuthorizationEndpoint string   `json:"device_authorization_endpoint,omitempty"`
	RegistrationEndpoint        string   `json:"registration_endpoint,omitempty"`
	CIMDSupported               bool     `json:"cimd_supported"`
	Discovered                  bool     `json:"discovered"`
	ScopesSupported             []string `json:"scopes_supported,omitempty"`
	TokenEndpointAuthMethods    []string `json:"token_endpoint_auth_methods,omitempty"`
	// PRMScopesSupported carries the protected-resource metadata
	// scopes_supported for one discovery run. It is request-local context
	// like Resource, never persisted: the metadata cache holds only the
	// authorization-server document.
	PRMScopesSupported []string `json:"-"`
}

// resolvedOAuthClient is the client credential selected for one OAuth run.
type resolvedOAuthClient struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
	Method       string
	AuthStyle    oauth2.AuthStyle
}

// oauthUserAgent identifies Oswald on OAuth metadata, registration, and
// token endpoints. Some WAF-fronted providers reject generic language
// clients; a product UA keeps discovery from cascading into guessed URLs.
const oauthUserAgent = "oswald-ai/1.0 (+mcp-oauth)"

type userAgentTransport struct{ base http.RoundTripper }

func (t userAgentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	clone := r.Clone(r.Context())
	if strings.TrimSpace(clone.Header.Get("User-Agent")) == "" {
		clone.Header.Set("User-Agent", oauthUserAgent)
	}
	return base.RoundTrip(clone)
}

// discoveryClient is a clean HTTP client for metadata endpoints. MCP
// credential headers are never sent to authorization servers.
func discoveryClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: userAgentTransport{base: http.DefaultTransport}}
}

// oauthHTTPClient wraps a base client (or the default discovery client) with
// the product User-Agent for token-adjacent POSTs.
func oauthHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		return discoveryClient()
	}
	clone := *base
	inner := clone.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}
	clone.Transport = userAgentTransport{base: inner}
	return &clone
}

// discoverOAuthMetadata locates the authorization server for an MCP endpoint
// through protected-resource metadata with a same-origin fallback, then loads
// authorization-server metadata using the operator-trusted HTTP/HTTPS policy.
func discoverOAuthMetadata(ctx context.Context, mcpURL string) (*serverMetadata, error) {
	return discoverOAuthMetadataWithClient(ctx, mcpURL, discoveryClient())
}

func discoverOAuthMetadataWithClient(ctx context.Context, mcpURL string, client *http.Client) (*serverMetadata, error) {
	return discoverOAuthMetadataCore(ctx, client, mcpURL, nil)
}

// discoverOAuthMetadataWithChallenge behaves like
// discoverOAuthMetadataWithClient but prefers a harvested resource_metadata
// URL (from a live challenge on the MCP endpoint) as the first
// protected-resource-metadata candidate.
func discoverOAuthMetadataWithChallenge(ctx context.Context, mcpURL string, client *http.Client, resourceMetadataURL string) (*serverMetadata, error) {
	return discoverOAuthMetadataCore(ctx, client, mcpURL, []string{resourceMetadataURL})
}

func discoverOAuthMetadataCore(ctx context.Context, client *http.Client, mcpURL string, extraPRMCandidates []string) (*serverMetadata, error) {
	issuer, resource, prmScopes, err := discoverIssuerFull(ctx, client, mcpURL, extraPRMCandidates)
	if err != nil {
		return nil, err
	}
	asm, device, err := fetchAuthorizationMetadata(ctx, client, issuer)
	if err != nil {
		return nil, fmt.Errorf("load authorization server metadata: %w", err)
	}
	meta := &serverMetadata{Issuer: issuer, Resource: resource, PRMScopesSupported: prmScopes}
	if asm != nil {
		meta.Discovered = true
		meta.AuthorizationEndpoint = asm.AuthorizationEndpoint
		meta.TokenEndpoint = asm.TokenEndpoint
		meta.RegistrationEndpoint = asm.RegistrationEndpoint
		meta.CIMDSupported = asm.ClientIDMetadataDocumentSupported
		meta.ScopesSupported = append([]string(nil), asm.ScopesSupported...)
		meta.TokenEndpointAuthMethods = append([]string(nil), asm.TokenEndpointAuthMethodsSupported...)
		meta.DeviceAuthorizationEndpoint = device
	} else {
		// Fallback predefined endpoints for servers without metadata discovery.
		meta.AuthorizationEndpoint = strings.TrimSuffix(issuer, "/") + "/authorize"
		meta.TokenEndpoint = strings.TrimSuffix(issuer, "/") + "/token"
		meta.RegistrationEndpoint = strings.TrimSuffix(issuer, "/") + "/register"
	}
	if (meta.AuthorizationEndpoint == "" && meta.DeviceAuthorizationEndpoint == "") || meta.TokenEndpoint == "" {
		return nil, fmt.Errorf("authorization server metadata is incomplete")
	}
	return meta, nil
}

func discoverIssuer(ctx context.Context, client *http.Client, mcpURL string) (string, string, error) {
	issuer, resource, _, err := discoverIssuerFull(ctx, client, mcpURL, nil)
	return issuer, resource, err
}

// discoverIssuerWithChallenge behaves like discoverIssuer but prefers a
// harvested resource_metadata URL (from a live challenge on the MCP
// endpoint) as the first protected-resource-metadata candidate. It also
// returns the selected document's scopes_supported, if advertised.
func discoverIssuerWithChallenge(ctx context.Context, client *http.Client, mcpURL, resourceMetadataURL string) (string, string, []string, error) {
	return discoverIssuerFull(ctx, client, mcpURL, []string{resourceMetadataURL})
}

func discoverIssuerFull(ctx context.Context, client *http.Client, mcpURL string, extraCandidates []string) (string, string, []string, error) {
	parsed, err := url.Parse(strings.TrimSpace(mcpURL))
	if err != nil || validateOAuthURL(mcpURL, false) != nil {
		return "", "", nil, fmt.Errorf("invalid MCP server URL")
	}
	resource := parsed.String()
	// A harvested resource_metadata URL is best-effort: any fetch failure,
	// absence, mismatch, or invalid issuer falls through to the derived
	// candidates below, never failing the flow.
	for _, extra := range extraCandidates {
		extra = strings.TrimSpace(extra)
		if extra == "" || validateOAuthURL(extra, false) != nil {
			continue
		}
		var prm oauthex.ProtectedResourceMetadata
		found, err := fetchOAuthMetadata(ctx, client, extra, &prm)
		if err != nil || !found || !oauthResourceCovers(prm.Resource, resource) {
			continue
		}
		if len(prm.AuthorizationServers) == 0 {
			return parsed.Scheme + "://" + parsed.Host, prm.Resource, append([]string(nil), prm.ScopesSupported...), nil
		}
		if err := checkIssuerURL(prm.AuthorizationServers[0]); err != nil {
			continue
		}
		return prm.AuthorizationServers[0], prm.Resource, append([]string(nil), prm.ScopesSupported...), nil
	}
	candidates := []string{
		parsed.Scheme + "://" + parsed.Host + "/.well-known/oauth-protected-resource/" + strings.TrimLeft(parsed.Path, "/"),
		parsed.Scheme + "://" + parsed.Host + "/.well-known/oauth-protected-resource",
	}
	for _, candidate := range candidates {
		var prm oauthex.ProtectedResourceMetadata
		found, err := fetchOAuthMetadata(ctx, client, candidate, &prm)
		if err != nil {
			return "", "", nil, err
		}
		if !found {
			continue
		}
		if !oauthResourceCovers(prm.Resource, resource) {
			return "", "", nil, fmt.Errorf("OAuth protected resource mismatch")
		}
		if len(prm.AuthorizationServers) == 0 {
			return parsed.Scheme + "://" + parsed.Host, prm.Resource, append([]string(nil), prm.ScopesSupported...), nil
		}
		if err := checkIssuerURL(prm.AuthorizationServers[0]); err != nil {
			return "", "", nil, err
		}
		return prm.AuthorizationServers[0], prm.Resource, append([]string(nil), prm.ScopesSupported...), nil
	}
	root := parsed.Scheme + "://" + parsed.Host
	if err := checkIssuerURL(root); err != nil {
		return "", "", nil, err
	}
	return root, resource, nil, nil
}

// oauthResourceCovers permits an exact resource or a same-origin base whose
// escaped path covers the endpoint at a segment boundary, never a sibling.
func oauthResourceCovers(resource, endpoint string) bool {
	if validateOAuthURL(resource, false) != nil || validateOAuthURL(endpoint, false) != nil {
		return false
	}
	r, _ := url.Parse(resource)
	e, _ := url.Parse(endpoint)
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	if r.Scheme != e.Scheme || !strings.EqualFold(r.Hostname(), e.Hostname()) || port(r) != port(e) {
		return false
	}
	for _, u := range []*url.URL{r, e} {
		for _, segment := range strings.Split(u.Path, "/") {
			if segment == "." || segment == ".." {
				return false
			}
		}
	}
	if resource == endpoint {
		return true
	}
	if r.RawQuery != "" || r.ForceQuery {
		return false
	}
	base := strings.TrimSuffix(r.EscapedPath(), "/")
	return e.EscapedPath() == base || strings.HasPrefix(e.EscapedPath(), base+"/")
}

func oauthResource(meta *serverMetadata, endpoint string) string {
	if meta.Resource != "" {
		return meta.Resource
	}
	return endpoint
}

// fetchOAuthMetadata deliberately accepts operator-trusted HTTP endpoints.
// Missing documents permit fallback; invalid documents and transport failures do not.
func fetchOAuthMetadata(ctx context.Context, client *http.Client, endpoint string, target any) (bool, error) {
	if err := validateOAuthURL(endpoint, false); err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("OAuth metadata request canceled: %w", ctx.Err())
		}
		return false, fmt.Errorf("OAuth metadata request failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
		return false, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("OAuth metadata returned unexpected status")
	}
	contentType := response.Header.Get("Content-Type")
	if strings.TrimSpace(contentType) != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil {
			return false, fmt.Errorf("OAuth metadata content type %q is invalid", contentType)
		}
		if mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json") {
			return false, fmt.Errorf("OAuth metadata content type %q is not JSON", mediaType)
		}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes+1))
	if err != nil || int64(len(data)) > maxMetadataBytes {
		return false, fmt.Errorf("OAuth metadata exceeds limit or could not be read")
	}
	if err := json.Unmarshal(data, target); err != nil {
		return false, fmt.Errorf("OAuth metadata JSON is invalid")
	}
	return true, nil
}

func fetchAuthorizationMetadata(ctx context.Context, client *http.Client, issuer string) (*oauthex.AuthServerMeta, string, error) {
	if err := checkIssuerURL(issuer); err != nil {
		return nil, "", err
	}
	base, _ := url.Parse(issuer)
	path := strings.Trim(base.Path, "/")
	paths := []string{"/.well-known/oauth-authorization-server", "/.well-known/openid-configuration"}
	if path != "" {
		paths = []string{paths[0] + "/" + path, paths[1] + "/" + path, "/" + path + paths[1]}
	}
	// Absent documents (404/410 on every candidate) permit the synthetic
	// fallback in the caller. Any malformed document, issuer mismatch, or
	// transport failure fails closed here and never falls back.
	for _, path := range paths {
		candidate := *base
		candidate.Path, candidate.RawPath = path, ""
		var document struct {
			oauthex.AuthServerMeta
			DeviceEndpoint string `json:"device_authorization_endpoint"`
		}
		found, err := fetchOAuthMetadata(ctx, client, candidate.String(), &document)
		if err != nil {
			return nil, "", err
		}
		if !found {
			continue
		}
		if !issuerMatches(document.Issuer, issuer) {
			if tolerated, keep := tolerateOriginIssuer(document.Issuer, issuer); tolerated {
				// Keep the advertised identifier for credential binding.
				document.Issuer = keep
			} else {
				return nil, "", fmt.Errorf("OAuth metadata issuer mismatch (advertised %q, document %q)", issuer, document.Issuer)
			}
		}
		if document.AuthorizationEndpoint != "" && !s256Advertised(document.CodeChallengeMethodsSupported) {
			return nil, "", fmt.Errorf("OAuth authorization server does not support S256 PKCE (advertised code_challenge_methods %q)", strings.Join(document.CodeChallengeMethodsSupported, " "))
		}
		for _, endpoint := range []string{document.AuthorizationEndpoint, document.TokenEndpoint, document.RegistrationEndpoint, document.DeviceEndpoint} {
			if endpoint != "" {
				if err := validateOAuthURL(endpoint, false); err != nil {
					return nil, "", err
				}
			}
		}
		if document.TokenEndpoint == "" {
			return nil, "", fmt.Errorf("OAuth token endpoint is missing")
		}
		return &document.AuthServerMeta, document.DeviceEndpoint, nil
	}
	return nil, "", nil
}

func checkIssuerURL(issuer string) error {
	return validateOAuthURL(issuer, true)
}

// normalizeIssuerURL trims a single trailing slash so an advertised
// https://host identifier and a document https://host/ identifier compare
// equal. Internal trailing state (empty path vs "/") is the only difference
// tolerated here.
func normalizeIssuerURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) > 0 && strings.HasSuffix(trimmed, "/") {
		if parsed, err := url.Parse(trimmed); err == nil && parsed.Path == "/" && parsed.RawQuery == "" {
			trimmed = strings.TrimSuffix(trimmed, "/")
		}
	}
	return trimmed
}

func issuerMatches(document, advertised string) bool {
	return normalizeIssuerURL(document) == normalizeIssuerURL(advertised)
}

// tolerateOriginIssuer accepts an origin-issued document for a path-scoped
// advertised issuer when both share the same origin and the document issuer
// is the bare origin. It returns the advertised identifier to keep for
// credential binding. Every other mismatch still fails closed.
func tolerateOriginIssuer(document, advertised string) (bool, string) {
	if issuerMatches(document, advertised) {
		return true, advertised
	}
	doc, errDoc := url.Parse(strings.TrimSpace(document))
	adv, errAdv := url.Parse(strings.TrimSpace(advertised))
	if errDoc != nil || errAdv != nil {
		return false, ""
	}
	if !strings.EqualFold(doc.Scheme, adv.Scheme) || !strings.EqualFold(doc.Hostname(), adv.Hostname()) {
		return false, ""
	}
	if doc.Port() != adv.Port() {
		return false, ""
	}
	if strings.Trim(doc.Path, "/") != "" {
		return false, ""
	}
	if strings.Trim(adv.Path, "/") == "" {
		return false, ""
	}
	return true, strings.TrimSpace(advertised)
}

// s256Advertised reports whether a browser authorization endpoint may use
// S256 PKCE. Servers that omit the list entirely are tried (many support
// S256 without advertising it); servers that advertise a list without S256
// fail closed in the caller.
func s256Advertised(methods []string) bool {
	if len(methods) == 0 {
		return true
	}
	for _, method := range methods {
		if method == "S256" {
			return true
		}
	}
	return false
}

func validateOAuthURL(raw string, issuer bool) error {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || (issuer && parsed.RawQuery != "") {
		return fmt.Errorf("OAuth URL is invalid")
	}
	return nil
}

// resolveOAuthClient selects the client credential for one OAuth run.
// Operator configuration wins, then a previously registered dynamic client,
// then CIMD with a loopback client ID unless a refusal was recorded.
func resolveOAuthClient(ctx context.Context, oauth *OAuthConfig, store *tokenStore, meta *serverMetadata, redirectURI string) (*resolvedOAuthClient, error) {
	if oauth == nil || meta == nil || strings.TrimSpace(redirectURI) == "" {
		return nil, fmt.Errorf("OAuth client cannot be resolved")
	}
	style := selectAuthStyle(meta.TokenEndpointAuthMethods, strings.TrimSpace(oauth.ClientSecret) != "")
	if strings.TrimSpace(oauth.ClientID) != "" {
		return &resolvedOAuthClient{ClientID: strings.TrimSpace(oauth.ClientID), ClientSecret: oauth.ClientSecret, RedirectURI: redirectURI, Method: clientMethodOperator, AuthStyle: style}, nil
	}
	if record, ok, err := store.LoadClient(ctx); err != nil {
		return nil, err
	} else if ok {
		for _, uri := range record.RedirectURIs {
			if uri == redirectURI {
				return &resolvedOAuthClient{ClientID: record.ClientID, ClientSecret: record.ClientSecret, RedirectURI: redirectURI, Method: record.Method, AuthStyle: selectAuthStyle(meta.TokenEndpointAuthMethods, record.ClientSecret != "")}, nil
			}
		}
	}
	disabled, err := store.CIMDDisabled(ctx)
	if err != nil {
		return nil, err
	}
	// An explicit metadata refusal skips CIMD without needing the marker;
	// servers without metadata are tried once and marked on refusal.
	if !disabled && (meta.CIMDSupported || !meta.Discovered) {
		if base, err := loopbackClientID(redirectURI); err == nil {
			return &resolvedOAuthClient{ClientID: base, RedirectURI: redirectURI, Method: clientMethodCIMD, AuthStyle: oauth2.AuthStyleInParams}, nil
		}
	}
	return nil, errNoCIMD
}

// errNoCIMD signals the caller to register dynamically and retry.
var errNoCIMD = fmt.Errorf("server requires dynamic client registration")

// loopbackClientID derives the CIMD-style public client identifier (scheme
// plus host, no path) from a loopback redirect URI.
func loopbackClientID(redirectURI string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(redirectURI))
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("redirect URI is invalid")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("redirect URI is invalid")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func selectAuthStyle(supported []string, hasSecret bool) oauth2.AuthStyle {
	if !hasSecret {
		return oauth2.AuthStyleInParams
	}
	if len(supported) == 0 {
		// Servers that omit the list while issuing a secret expect the
		// secret in the body.
		return oauth2.AuthStyleInParams
	}
	onlyNone := true
	for _, offered := range supported {
		if offered != "none" {
			onlyNone = false
			break
		}
	}
	if onlyNone {
		// A secret with only "none" advertised cannot use the default;
		// coerce to client_secret_post (body) which strict servers accept.
		return oauth2.AuthStyleInParams
	}
	for _, method := range []string{"client_secret_post", "client_secret_basic", "none"} {
		for _, offered := range supported {
			if offered == method {
				switch method {
				case "client_secret_post", "none":
					return oauth2.AuthStyleInParams
				default:
					return oauth2.AuthStyleInHeader
				}
			}
		}
	}
	return oauth2.AuthStyleAutoDetect
}

// performDCR registers a native loopback client and persists the credential.
func performDCR(ctx context.Context, store *tokenStore, meta *serverMetadata, redirectURI string, scopes []string) (*resolvedOAuthClient, error) {
	if strings.TrimSpace(meta.RegistrationEndpoint) == "" {
		return nil, fmt.Errorf("server supports neither client metadata documents nor dynamic registration")
	}
	registration, err := oauthex.RegisterClient(ctx, meta.RegistrationEndpoint, &oauthex.ClientRegistrationMetadata{
		RedirectURIs:            []string{redirectURI},
		ApplicationType:         "native",
		ClientName:              "oswald-ai",
		Scope:                   strings.Join(scopes, " "),
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
	}, oauthHTTPClient(nil))
	if err != nil {
		return nil, fmt.Errorf("register OAuth client: %w", err)
	}
	record := ClientRecord{ClientID: registration.ClientID, ClientSecret: registration.ClientSecret, RedirectURIs: []string{redirectURI}, Method: clientMethodDCR}
	if err := store.SaveClient(ctx, record); err != nil {
		return nil, err
	}
	style := oauth2.AuthStyleInHeader
	if registration.TokenEndpointAuthMethod == "client_secret_post" || registration.TokenEndpointAuthMethod == "none" {
		style = oauth2.AuthStyleInParams
	}
	if registration.ClientSecret != "" && strings.TrimSpace(registration.TokenEndpointAuthMethod) == "" {
		// Servers that issue a secret without naming a method expect it in
		// the body; the "none" default would break the exchange.
		style = oauth2.AuthStyleInParams
	}
	return &resolvedOAuthClient{ClientID: registration.ClientID, ClientSecret: registration.ClientSecret, RedirectURI: redirectURI, Method: clientMethodDCR, AuthStyle: style}, nil
}

// splitChallengeParams splits one WWW-Authenticate header value on commas
// outside double quotes, so quoted scope lists containing spaces survive.
func splitChallengeParams(header string) []string {
	var parts []string
	var cur strings.Builder
	inQuotes := false
	escaped := false
	for _, r := range header {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case inQuotes && r == '\\':
			cur.WriteRune(r)
			escaped = true
		case r == '"':
			cur.WriteRune(r)
			inQuotes = !inQuotes
		case r == ',' && !inQuotes:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(parts, cur.String())
}

// parseWWWAuthenticateParams parses one WWW-Authenticate header value (or
// several joined with ", ") into lowercase parameter names. Keys are
// case-insensitive; values may be tokens or quoted strings, which are
// unquoted; a leading auth-scheme token (Bearer, Basic, ...) is stripped
// from the first parameter name. The first occurrence of a name wins. It
// never fails: garbage yields an empty map. Callers must log only
// presence/counts, never values.
func parseWWWAuthenticateParams(header string) map[string]string {
	out := map[string]string{}
	for _, part := range splitChallengeParams(header) {
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			// Bare scheme token or token68: no parameters.
			continue
		}
		name = strings.TrimSpace(name)
		if i := strings.LastIndexAny(name, " \t"); i >= 0 {
			name = strings.TrimSpace(name[i+1:])
		}
		key := strings.ToLower(name)
		if key == "" {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") {
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			} else {
				value = strings.Trim(value, "\"")
			}
		}
		value = strings.TrimSpace(value)
		if _, exists := out[key]; !exists {
			out[key] = value
		}
	}
	return out
}

// parseWWWAuthenticateScope returns the scope parameter of one
// WWW-Authenticate header value (or several joined with ", "), unquoted and
// trimmed, or "" when absent.
func parseWWWAuthenticateScope(header string) string {
	return parseWWWAuthenticateParams(header)["scope"]
}

// parseWWWAuthenticateField returns one WWW-Authenticate parameter across
// all header values of a live response, or "" when absent. It is nil-safe.
func parseWWWAuthenticateField(resp *http.Response, field string) string {
	if resp == nil {
		return ""
	}
	combined := strings.Join(resp.Header.Values("WWW-Authenticate"), ", ")
	if strings.TrimSpace(combined) == "" {
		return ""
	}
	return parseWWWAuthenticateParams(combined)[strings.ToLower(field)]
}

// extractChallengeScope returns the scope parameter across all
// WWW-Authenticate header values of a live response, or "" on any absence.
// It is nil-safe; the caller decides which statuses are relevant (401 and
// 403 challenges are accepted).
func extractChallengeScope(resp *http.Response) string {
	return parseWWWAuthenticateField(resp, "scope")
}

// unionScopes joins a previous scope string with a challenged scope string,
// preserving previous order and appending challenged values not already
// present. ""+"" yields "".
func unionScopes(previous, challenged string) string {
	var out []string
	seen := map[string]bool{}
	for _, scope := range strings.Fields(previous) {
		if !seen[scope] {
			seen[scope] = true
			out = append(out, scope)
		}
	}
	for _, scope := range strings.Fields(challenged) {
		if !seen[scope] {
			seen[scope] = true
			out = append(out, scope)
		}
	}
	return strings.Join(out, " ")
}

// challengeScopesCovered reports whether every challenged scope token is
// already in the stored grant. An empty challenge is vacuously covered.
func challengeScopesCovered(stored []string, challenged string) bool {
	fields := strings.Fields(challenged)
	if len(fields) == 0 {
		return true
	}
	set := make(map[string]bool, len(stored))
	for _, scope := range stored {
		set[scope] = true
	}
	for _, scope := range fields {
		if !set[scope] {
			return false
		}
	}
	return true
}

// selectEffectiveScopes picks the request scope with an exact priority:
// configured scopes verbatim, else the live challenge scope, else PRM
// scopes_supported in advertised order, else AS scopes_supported in
// advertised order, else nil (omitted everywhere so the server grants its
// default set). offline_access is appended per needsOfflineAccess.
func selectEffectiveScopes(requested []string, challengeScope string, meta *serverMetadata) []string {
	var selected []string
	switch {
	case len(requested) > 0:
		selected = append([]string(nil), requested...)
	case strings.TrimSpace(challengeScope) != "":
		selected = strings.Fields(challengeScope)
	case meta != nil && len(meta.PRMScopesSupported) > 0:
		selected = append([]string(nil), meta.PRMScopesSupported...)
	case meta != nil && len(meta.ScopesSupported) > 0:
		selected = append([]string(nil), meta.ScopesSupported...)
	}
	return withOfflineAccess(selected, meta)
}

// scopeSelectionSource names the rule that selectEffectiveScopes applies,
// for count-only diagnostics. It takes the same inputs.
func scopeSelectionSource(requested []string, challengeScope string, meta *serverMetadata) string {
	switch {
	case len(requested) > 0:
		return "config"
	case strings.TrimSpace(challengeScope) != "":
		return "challenge"
	case meta != nil && len(meta.PRMScopesSupported) > 0:
		return "prm"
	case meta != nil && len(meta.ScopesSupported) > 0:
		return "as"
	default:
		return "omitted"
	}
}

// needsOfflineAccess reports whether offline_access must be appended: a
// scope was selected, the authorization server advertises offline_access,
// and it is not already present.
func needsOfflineAccess(selected []string, meta *serverMetadata) bool {
	if len(selected) == 0 || meta == nil {
		return false
	}
	for _, scope := range selected {
		if scope == "offline_access" {
			return false
		}
	}
	for _, scope := range meta.ScopesSupported {
		if scope == "offline_access" {
			return true
		}
	}
	return false
}

// withOfflineAccess appends offline_access (no duplicates) when
// needsOfflineAccess holds. The DCR grant list always contains
// refresh_token, which is what makes the offline_access request meaningful;
// operator-client grants are assumed to include it as well.
func withOfflineAccess(selected []string, meta *serverMetadata) []string {
	if !needsOfflineAccess(selected, meta) {
		return selected
	}
	return append(append([]string(nil), selected...), "offline_access")
}

// cliEffectiveScopes computes the CLI request scope: automatic selection,
// plus the cross-process step-up union of the stored granted set with a
// live challenge when the operator left scopes empty. Configured scopes
// always win verbatim (subject only to the offline_access rule).
func cliEffectiveScopes(configured, stored []string, challengeScope string, meta *serverMetadata) []string {
	if len(configured) == 0 && len(stored) > 0 && strings.TrimSpace(challengeScope) != "" {
		return withOfflineAccess(strings.Fields(unionScopes(strings.Join(stored, " "), challengeScope)), meta)
	}
	return selectEffectiveScopes(configured, challengeScope, meta)
}

// fetchChallenge issues one unauthenticated GET against the MCP endpoint
// and harvests the RFC 6750 challenge: the scope parameter and the
// resource_metadata URL of a 401/403 WWW-Authenticate response. It never
// sends secrets and never fails the flow: any error, absence, or
// non-challenge status yields "","" so selection falls through to
// advertised/omitted scopes.
func fetchChallenge(ctx context.Context, client *http.Client, mcpURL string) (challengeScope, resourceMetadataURL string) {
	if client == nil {
		client = discoveryClient()
	}
	endpoint := strings.TrimSpace(mcpURL)
	if validateOAuthURL(endpoint, false) != nil {
		return "", ""
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", ""
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return "", ""
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxMetadataBytes+1))
	if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden {
		return "", ""
	}
	return extractChallengeScope(response), parseWWWAuthenticateField(response, "resource_metadata")
}
