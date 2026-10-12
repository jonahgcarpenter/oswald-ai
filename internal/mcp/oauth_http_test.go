package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type oauthTestTransport func(*http.Request) (*http.Response, error)

func (f oauthTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Map a non-loopback authority to a synthetic server without weakening the
// production client's dialing or TLS policy.
func oauthMappedClient(server *httptest.Server) *http.Client {
	return &http.Client{Transport: oauthTestTransport(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		u := *r.URL
		u.Scheme = "http"
		u.Host = strings.TrimPrefix(server.URL, "http://")
		copy.URL = &u
		return http.DefaultTransport.RoundTrip(copy)
	})}
}

func TestOAuthNonLoopbackDiscovery(t *testing.T) {
	for _, origin := range []string{"http://192.168.4.52:8123", "http://homeassistant.local:8123", "https://ha.example"} {
		t.Run(origin, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("credential sent during discovery")
				}
				switch r.URL.Path {
				case "/.well-known/oauth-protected-resource/api/mcp":
					writeJSON(w, map[string]any{"resource": origin + "/api/mcp", "authorization_servers": []string{origin + "/auth"}})
				case "/.well-known/oauth-authorization-server/auth":
					writeJSON(w, map[string]any{"issuer": origin + "/auth", "authorization_endpoint": origin + "/authorize", "token_endpoint": origin + "/token", "device_authorization_endpoint": origin + "/device", "code_challenge_methods_supported": []string{"S256"}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			meta, err := discoverOAuthMetadataWithClient(context.Background(), origin+"/api/mcp", oauthMappedClient(server))
			if err != nil || meta.Issuer != origin+"/auth" || meta.DeviceAuthorizationEndpoint != origin+"/device" {
				t.Fatalf("metadata=%v error=%v", meta, err)
			}
		})
	}
}

func TestOAuthMetadataFailsClosed(t *testing.T) {
	for _, body := range []string{
		`{"issuer":"http://different.example","token_endpoint":"http://ha.local/token"}`,
		`{"issuer":"http://ha.local","token_endpoint":"javascript:alert(1)","device_authorization_endpoint":"http://ha.local/device"}`,
		`{"issuer":"http://ha.local","token_endpoint":"http://user:secret@ha.local/token","device_authorization_endpoint":"http://ha.local/device"}`,
		`{"issuer":"http://ha.local","authorization_endpoint":"http://ha.local/auth","token_endpoint":"http://ha.local/token","code_challenge_methods_supported":["plain"]}`,
		`null`, `invalid`, strings.Repeat("x", maxMetadataBytes+1),
	} {
		t.Run(fmt.Sprintf("body_%d", len(body)), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "oauth-protected-resource") {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			if _, err := discoverOAuthMetadataWithClient(context.Background(), "http://ha.local/mcp", oauthMappedClient(server)); err == nil {
				t.Fatal("invalid metadata permitted fallback")
			}
		})
	}
}

func TestOAuthResourceMismatchAndMissingMetadata(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mismatch {
				writeJSON(w, map[string]any{"resource": "http://other.local/mcp"})
			} else {
				http.NotFound(w, r)
			}
		}))
		meta, err := discoverOAuthMetadataWithClient(context.Background(), "http://ha.local/mcp", oauthMappedClient(server))
		server.Close()
		if mismatch && err == nil {
			t.Fatal("resource mismatch accepted")
		}
		if !mismatch && (err != nil || meta.TokenEndpoint != "http://ha.local/token" || meta.Discovered) {
			t.Fatalf("fallback metadata=%v error=%v", meta, err)
		}
	}
}

func TestOAuthHTTPExchangeAndRefresh(t *testing.T) {
	const origin = "http://192.168.4.52:8123"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("client_id") != "test-client" {
			t.Error("client identity missing")
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code_verifier") != "test-verifier" || r.Form.Get("resource") != origin+"/mcp" {
				t.Error("PKCE verifier or resource missing")
			}
		case "refresh_token":
			if r.Form.Get("refresh_token") != "refresh-canary" {
				t.Error("refresh token missing")
			}
		default:
			t.Error("unexpected grant")
		}
		writeJSON(w, map[string]any{"access_token": "access-canary", "refresh_token": "refresh-canary", "token_type": "Bearer", "expires_in": 3600})
	}))
	defer server.Close()
	client := &resolvedOAuthClient{ClientID: "test-client", AuthStyle: oauth2.AuthStyleInParams}
	meta := &serverMetadata{Issuer: origin, TokenEndpoint: origin + "/token"}
	ctx := context.Background()
	tokens, err := exchangeCode(ctx, client, meta, nil, origin+"/mcp", "test-code", "test-verifier", oauthMappedClient(server))
	if err != nil {
		t.Fatal(err)
	}
	store, _ := testOAuthStore(t)
	tokens.Expiry = time.Now().Add(-time.Hour)
	if err := store.SaveTokens(ctx, *tokens); err != nil {
		t.Fatal(err)
	}
	refreshed, err := refreshOAuthToken(ctx, store, meta, client, nil, oauthMappedClient(server))
	if err != nil || refreshed.Issuer != origin {
		t.Fatalf("refresh=%v error=%v", refreshed, err)
	}
}

func TestOAuthDiscoveryErrorsDoNotFallback(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		_, err := discoverOAuthMetadataWithClient(context.Background(), "http://ha.local/mcp", oauthMappedClient(server))
		server.Close()
		if err == nil {
			t.Fatal("HTTP failure permitted fallback")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Transport: oauthTestTransport(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })}
	if _, err := discoverOAuthMetadataWithClient(ctx, "http://ha.local/mcp", client); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestOAuthResourceBaseBoundaries(t *testing.T) {
	for _, tc := range []struct {
		resource, endpoint string
		want               bool
	}{
		{"http://ha.local", "http://ha.local/api/mcp", true},
		{"http://ha.local/", "http://ha.local/api/mcp", true},
		{"http://ha.local/api", "http://ha.local/api/mcp", true},
		{"http://ha.local/api/", "http://ha.local/api/mcp", true},
		{"http://ha.local/api/mcp", "http://ha.local/api/mcp", true},
		{"http://HA.local:80", "http://ha.local/api/mcp", true},
		{"http://other.local", "http://ha.local/api/mcp", false},
		{"https://ha.local", "http://ha.local/api/mcp", false},
		{"http://ha.local:8123", "http://ha.local:8124/api/mcp", false},
		{"http://ha.local/api", "http://ha.local/apix/mcp", false},
		{"http://ha.local/api", "http://ha.local/api%2Fmcp", false},
		{"http://ha.local/api", "http://ha.local/api/../other", false},
		{"http://ha.local/api", "http://ha.local/api/%2e%2e/other", false},
		{"http://ha.local/api/mcp", "http://ha.local/api", false},
		{"http://user:secret@ha.local", "http://ha.local/api/mcp", false},
		{"http://ha.local?tenant=other", "http://ha.local/api/mcp", false},
	} {
		if got := oauthResourceCovers(tc.resource, tc.endpoint); got != tc.want {
			t.Errorf("covers(%q, %q)=%t, want %t", tc.resource, tc.endpoint, got, tc.want)
		}
	}
}

func TestOAuthOriginResourceBrowserRoundTrip(t *testing.T) {
	fake := newFakeAuthorizationServer(t, true)
	defer fake.close()
	fake.rootResource = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	meta, err := discoverOAuthMetadata(ctx, fake.mcpURL())
	if err != nil {
		t.Fatal(err)
	}
	if meta.Resource != fake.server.URL {
		t.Fatal("origin resource not retained")
	}
	store, _ := testOAuthStore(t)
	var printed syncBuffer
	done := make(chan error, 1)
	go func() {
		_, err := RunBrowserFlow(ctx, store, &OAuthConfig{ClientID: "operator-client", Timeout: 5 * time.Second}, meta, fake.mcpURL(), FlowOptions{NoBrowser: true, Stdout: &printed})
		done <- err
	}()
	var requestErr error
	for ctx.Err() == nil {
		var authURL string
		for _, line := range strings.Split(printed.String(), "\n") {
			if strings.HasPrefix(line, fake.server.URL+"/authorize?") {
				authURL = line
				break
			}
		}
		if authURL != "" {
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, authURL, nil)
			response, err := discoveryClient().Do(request)
			requestErr = err
			if response != nil {
				response.Body.Close()
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	if requestErr != nil {
		cancel()
	}
	if err := <-done; err != nil || requestErr != nil {
		t.Fatalf("browser error=%v request error=%v", err, requestErr)
	}
	if err := store.SaveMetadata(ctx, *meta); err != nil {
		t.Fatal(err)
	}
	cached, _, err := store.LoadMetadata(ctx)
	if err != nil || cached.Resource != "" {
		t.Fatal("request-local resource changed persisted metadata")
	}
}
