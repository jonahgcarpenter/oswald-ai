package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
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
}

// resolvedOAuthClient is the client credential selected for one OAuth run.
type resolvedOAuthClient struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
	Method       string
	AuthStyle    oauth2.AuthStyle
}

// discoveryClient is a clean HTTP client for metadata endpoints. MCP
// credential headers are never sent to authorization servers.
func discoveryClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// discoverOAuthMetadata locates the authorization server for an MCP endpoint
// through protected-resource metadata with a same-origin fallback, then loads
// authorization-server metadata using the operator-trusted HTTP/HTTPS policy.
func discoverOAuthMetadata(ctx context.Context, mcpURL string) (*serverMetadata, error) {
	return discoverOAuthMetadataWithClient(ctx, mcpURL, discoveryClient())
}

func discoverOAuthMetadataWithClient(ctx context.Context, mcpURL string, client *http.Client) (*serverMetadata, error) {
	issuer, resource, err := discoverIssuer(ctx, client, mcpURL)
	if err != nil {
		return nil, err
	}
	asm, device, err := fetchAuthorizationMetadata(ctx, client, issuer)
	if err != nil {
		return nil, fmt.Errorf("load authorization server metadata: %w", err)
	}
	meta := &serverMetadata{Issuer: issuer, Resource: resource}
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
	parsed, err := url.Parse(strings.TrimSpace(mcpURL))
	if err != nil || validateOAuthURL(mcpURL, false) != nil {
		return "", "", fmt.Errorf("invalid MCP server URL")
	}
	resource := parsed.String()
	candidates := []string{
		parsed.Scheme + "://" + parsed.Host + "/.well-known/oauth-protected-resource/" + strings.TrimLeft(parsed.Path, "/"),
		parsed.Scheme + "://" + parsed.Host + "/.well-known/oauth-protected-resource",
	}
	for _, candidate := range candidates {
		var prm oauthex.ProtectedResourceMetadata
		found, err := fetchOAuthMetadata(ctx, client, candidate, &prm)
		if err != nil {
			return "", "", err
		}
		if !found {
			continue
		}
		if !oauthResourceCovers(prm.Resource, resource) {
			return "", "", fmt.Errorf("OAuth protected resource mismatch")
		}
		if len(prm.AuthorizationServers) == 0 {
			return parsed.Scheme + "://" + parsed.Host, prm.Resource, nil
		}
		if err := checkIssuerURL(prm.AuthorizationServers[0]); err != nil {
			return "", "", err
		}
		return prm.AuthorizationServers[0], prm.Resource, nil
	}
	root := parsed.Scheme + "://" + parsed.Host
	if err := checkIssuerURL(root); err != nil {
		return "", "", err
	}
	return root, resource, nil
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
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return false, fmt.Errorf("OAuth metadata content type is invalid")
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
		if document.Issuer != issuer {
			return nil, "", fmt.Errorf("OAuth metadata issuer mismatch")
		}
		if document.AuthorizationEndpoint != "" {
			s256 := false
			for _, method := range document.CodeChallengeMethodsSupported {
				s256 = s256 || method == "S256"
			}
			if !s256 {
				return nil, "", fmt.Errorf("OAuth authorization server does not support S256 PKCE")
			}
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
		RedirectURIs:    []string{redirectURI},
		ApplicationType: "native",
		ClientName:      "oswald-ai",
		Scope:           strings.Join(scopes, " "),
	}, discoveryClient())
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
	return &resolvedOAuthClient{ClientID: registration.ClientID, ClientSecret: registration.ClientSecret, RedirectURI: redirectURI, Method: clientMethodDCR, AuthStyle: style}, nil
}
