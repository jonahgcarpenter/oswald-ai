package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSubscriptionConfig(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSubscriptionChatGPTSelected(t *testing.T) {
	root := writeSubscriptionConfig(t, "providers:\n  sub:\n    auth: chatgpt-oauth\nmodel:\n  provider: custom:sub\n  default: gpt-5.6-luna\n  context_length: 32768\nplatforms:\n  api:\n    enabled: true\n    extra:\n      api_host: \"127.0.0.1\"\n      api_port: 12345\n")
	cfg, err := Validate(root)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.LLMGatewayAuthKind != ProviderAuthChatGPT {
		t.Fatalf("auth kind = %q", cfg.LLMGatewayAuthKind)
	}
	if cfg.LLMGatewayAPIKey != "" {
		t.Fatalf("subscription must not carry a static key")
	}
	if cfg.LLMGatewayURL != DefaultChatGPTAPI {
		t.Fatalf("chatgpt must pin the Codex backend, got %q", cfg.LLMGatewayURL)
	}
	ep, ok := cfg.ProviderEndpoints["sub"]
	if !ok || ep.AuthKind != ProviderAuthChatGPT {
		t.Fatalf("endpoint = %+v ok=%v", ep, ok)
	}
	if ep.URL != DefaultChatGPTAPI {
		t.Fatalf("endpoint URL = %q", ep.URL)
	}
}

func TestSubscriptionClaudeNoAPI(t *testing.T) {
	root := writeSubscriptionConfig(t, "providers:\n  cl:\n    auth: claude-sdk\nmodel:\n  provider: custom:cl\n  default: claude-sonnet-4-6\n  context_length: 32768\nplatforms:\n  api:\n    enabled: true\n    extra:\n      api_host: \"127.0.0.1\"\n      api_port: 12345\n")
	cfg, err := Validate(root)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.LLMGatewayAuthKind != ProviderAuthClaudeSDK || cfg.LLMGatewayURL != "" {
		t.Fatalf("claude endpoint = %+v", cfg)
	}
	ep, ok := cfg.ProviderEndpoints["cl"]
	if !ok || ep.AuthKind != ProviderAuthClaudeSDK {
		t.Fatalf("endpoint = %+v ok=%v", ep, ok)
	}
}

func TestSubscriptionRejectsKey(t *testing.T) {
	root := writeSubscriptionConfig(t, "providers:\n  sub:\n    api: https://chatgpt.example.com\n    auth: chatgpt-oauth\n    key: synthetic-key\nmodel:\n  provider: custom:sub\n  default: gpt-5.5\nplatforms:\n  api:\n    enabled: true\n    extra:\n      api_host: \"127.0.0.1\"\n      api_port: 12345\n")
	if _, err := Validate(root); err == nil {
		t.Fatal("accepted static key on subscription provider")
	}
}

func TestSubscriptionRejectsBadAuth(t *testing.T) {
	root := writeSubscriptionConfig(t, "providers:\n  sub:\n    api: https://chatgpt.example.com\n    auth: oauth\nmodel:\n  provider: custom:sub\n  default: gpt-5.5\nplatforms:\n  api:\n    enabled: true\n    extra:\n      api_host: \"127.0.0.1\"\n      api_port: 12345\n")
	if _, err := Validate(root); err == nil {
		t.Fatal("accepted unknown auth kind")
	}
}

func TestSubscriptionChatGPTRejectsAPI(t *testing.T) {
	root := writeSubscriptionConfig(t, "providers:\n  sub:\n    api: https://api.openai.com/v1\n    auth: chatgpt-oauth\nmodel:\n  provider: custom:sub\n  default: gpt-5.6-luna\nplatforms:\n  api:\n    enabled: true\n    extra:\n      api_host: \"127.0.0.1\"\n      api_port: 12345\n")
	if _, err := Validate(root); err == nil {
		t.Fatal("accepted api override on chatgpt-oauth provider")
	}
}

func TestSetConfigLeavesConvertsToSubscription(t *testing.T) {
	root := writeSubscriptionConfig(t, "providers:\n  sub:\n    api: https://example.com/v1\n    key: synthetic-key\nmodel:\n  provider: custom:sub\n  default: m\nplatforms:\n  api:\n    enabled: true\n    extra:\n      api_host: \"127.0.0.1\"\n      api_port: 12345\n")
	sets := map[string]string{"providers.sub.auth": "chatgpt-oauth"}
	unsets := []string{"providers.sub.api", "providers.sub.key"}
	if err := SetConfigLeaves(root, "default", sets, unsets); err != nil {
		t.Fatalf("leaves = err %v", err)
	}
	cfg, err := Validate(root)
	if err != nil {
		t.Fatalf("validate after leaves: %v", err)
	}
	ep, ok := cfg.ProviderEndpoints["sub"]
	if !ok || ep.AuthKind != ProviderAuthChatGPT || ep.URL != DefaultChatGPTAPI || ep.APIKey != "" {
		t.Fatalf("endpoint = %+v ok=%v", ep, ok)
	}
}

func TestSetConfigLeavesRejectsAtomically(t *testing.T) {
	root := writeSubscriptionConfig(t, "providers:\n  sub:\n    api: https://example.com/v1\n    key: synthetic-key\nmodel:\n  provider: custom:sub\n  default: m\nplatforms:\n  api:\n    enabled: true\n    extra:\n      api_host: \"127.0.0.1\"\n      api_port: 12345\n")
	before, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Setting the subscription auth without clearing the static leaves
	// fails the single staged validation; nothing reaches disk.
	sets := map[string]string{"providers.sub.auth": "chatgpt-oauth"}
	if err := SetConfigLeaves(root, "default", sets, nil); err == nil {
		t.Fatal("accepted subscription auth with lingering key")
	}
	after, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("file mutated on failed batch")
	}
}
