package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deviceFake emulates just enough of an authorization server for the device
// flow: metadata, device authorization, and an immediately-approved token poll.
func deviceFake(t *testing.T) *httptest.Server {
	t.Helper()
	var issuer string
	encode := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	mux := http.NewServeMux()
	metadata := func(w http.ResponseWriter) {
		encode(w, map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "/authorize",
			"token_endpoint":                        issuer + "/token",
			"device_authorization_endpoint":         issuer + "/device",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:device_code"},
			"code_challenge_methods_supported":      []string{"S256"},
			"client_id_metadata_document_supported": true,
			"scopes_supported":                      []string{"read", "write"},
		})
	}
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		encode(w, map[string]any{"resource": issuer + "/mcp", "authorization_servers": []string{issuer}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) { metadata(w) })
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		encode(w, map[string]any{
			"device_code": "device-code-1", "user_code": "USER-CODE",
			"verification_uri": issuer + "/activate", "expires_in": 300, "interval": 0,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.PostForm.Get("device_code") != "device-code-1" {
			w.WriteHeader(http.StatusBadRequest)
			encode(w, map[string]any{"error": "invalid_grant"})
			return
		}
		encode(w, map[string]any{"access_token": "cli-access", "token_type": "Bearer", "refresh_token": "cli-refresh", "expires_in": 3600})
	})
	server := httptest.NewServer(mux)
	issuer = server.URL
	return server
}

func cliConfigRoot(t *testing.T, mcpBlock string) string {
	t.Helper()
	t.Setenv("PROFILE_TEST_MODEL_KEY", "synthetic-process-credential")
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	config := `providers:
  fake:
    api: "http://localhost:9999/v1"
    key: "${PROFILE_TEST_MODEL_KEY}"
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
platforms:
  api:
    enabled: true
    extra:
      api_host: "127.0.0.1"
      api_port: 12345
` + mcpBlock
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestAuthDeviceFlowStoresTokens(t *testing.T) {
	fake := deviceFake(t)
	defer fake.Close()
	root := cliConfigRoot(t, "mcp_servers:\n  login:\n    url: "+fake.URL+"/mcp\n    auth: oauth\n    oauth:\n      flow: device\n")
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), root, []string{"auth", "login"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d (stderr=%q)", code, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "mcp", "login", "login.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		AccessToken string `json:"access_token"`
		Issuer      string `json:"issuer"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "cli-access" || stored.Issuer != fake.URL {
		t.Fatalf("stored = %+v", stored)
	}
	if !strings.Contains(stdout.String(), "Authorized login") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "OAuth scope selection: as (2 scope(s)).") {
		t.Fatalf("missing scope selection: stdout = %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "cli-access") || strings.Contains(stderr.String(), "cli-access") {
		t.Fatal("credential leaked into output")
	}
}

func TestAuthRejectsBadInput(t *testing.T) {
	fake := deviceFake(t)
	defer fake.Close()
	root := cliConfigRoot(t, "mcp_servers:\n  login:\n    url: "+fake.URL+"/mcp\n    auth: oauth\n    oauth:\n      flow: device\n  plain:\n    url: https://example.com/mcp\n")
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"no server", []string{"auth"}, exitUsage},
		{"unknown server", []string{"auth", "missing"}, exitError},
		{"non-oauth server", []string{"auth", "plain"}, exitError},
		{"unknown profile", []string{"auth", "--profile", "missing", "login"}, exitError},
		{"server first unknown profile", []string{"auth", "login", "--profile", "missing"}, exitError},
		{"server between flags", []string{"auth", "--flow", "device", "login", "--profile", "missing"}, exitError},
		{"bad flow", []string{"auth", "--flow", "implicit", "login"}, exitError},
		{"unknown command", []string{"frobnicate"}, exitUsage},
		{"help", []string{"help"}, exitOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := Run(context.Background(), root, tc.args, strings.NewReader(""), &stdout, &stderr); got != tc.want {
				t.Fatalf("exit = %d, want %d (stderr=%q)", got, tc.want, stderr.String())
			}
		})
	}
}
