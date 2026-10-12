package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

const maxMetadataBytes = 64 * 1024

// serverMetadata is the discovered OAuth configuration for one MCP server.
// It is cached in <server>.meta.json.
type serverMetadata struct {
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
// authorization-server metadata the same way the SDK does.
func discoverOAuthMetadata(ctx context.Context, mcpURL string) (*serverMetadata, error) {
	client := discoveryClient()
	issuer, err := discoverIssuer(ctx, client, mcpURL)
	if err != nil {
		return nil, err
	}
	asm, err := sdkauth.GetAuthServerMetadata(ctx, issuer, client)
	if err != nil {
		return nil, fmt.Errorf("load authorization server metadata: %w", err)
	}
	meta := &serverMetadata{Issuer: issuer}
	if asm != nil {
		meta.Discovered = true
		meta.AuthorizationEndpoint = asm.AuthorizationEndpoint
		meta.TokenEndpoint = asm.TokenEndpoint
		meta.RegistrationEndpoint = asm.RegistrationEndpoint
		meta.CIMDSupported = asm.ClientIDMetadataDocumentSupported
		meta.ScopesSupported = append([]string(nil), asm.ScopesSupported...)
		meta.TokenEndpointAuthMethods = append([]string(nil), asm.TokenEndpointAuthMethodsSupported...)
	} else {
		// Fallback predefined endpoints for servers without metadata discovery.
		meta.AuthorizationEndpoint = strings.TrimSuffix(issuer, "/") + "/authorize"
		meta.TokenEndpoint = strings.TrimSuffix(issuer, "/") + "/token"
		meta.RegistrationEndpoint = strings.TrimSuffix(issuer, "/") + "/register"
	}
	if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" {
		return nil, fmt.Errorf("authorization server metadata is incomplete")
	}
	if device, err := discoverDeviceEndpoint(ctx, client, issuer); err != nil {
		return nil, err
	} else if device != "" {
		meta.DeviceAuthorizationEndpoint = device
	}
	return meta, nil
}

func discoverIssuer(ctx context.Context, client *http.Client, mcpURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(mcpURL))
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid MCP server URL")
	}
	resource := parsed.String()
	candidates := []string{
		parsed.Scheme + "://" + parsed.Host + "/.well-known/oauth-protected-resource/" + strings.TrimLeft(parsed.Path, "/"),
		parsed.Scheme + "://" + parsed.Host + "/.well-known/oauth-protected-resource",
	}
	for _, candidate := range candidates {
		prm, err := oauthex.GetProtectedResourceMetadata(ctx, candidate, resource, client)
		if err != nil || prm == nil || len(prm.AuthorizationServers) == 0 {
			continue
		}
		return prm.AuthorizationServers[0], nil
	}
	root := parsed.Scheme + "://" + parsed.Host
	if err := checkIssuerURL(root); err != nil {
		return "", err
	}
	return root, nil
}

// discoverDeviceEndpoint reads the device_authorization_endpoint, which the
// SDK metadata struct does not expose. A missing field is not an error;
// device flow requires it later.
func discoverDeviceEndpoint(ctx context.Context, client *http.Client, issuer string) (string, error) {
	metaURL := strings.TrimSuffix(issuer, "/") + "/.well-known/oauth-authorization-server"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return "", nil
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes+1))
	if err != nil || int64(len(data)) > maxMetadataBytes {
		return "", nil
	}
	var raw struct {
		DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", nil
	}
	endpoint := strings.TrimSpace(raw.DeviceAuthorizationEndpoint)
	if endpoint == "" {
		return "", nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return "", fmt.Errorf("device authorization endpoint is invalid")
	}
	return endpoint, nil
}

func checkIssuerURL(issuer string) error {
	parsed, err := url.Parse(strings.TrimSpace(issuer))
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("authorization server issuer is invalid")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return fmt.Errorf("authorization server issuer is invalid")
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
