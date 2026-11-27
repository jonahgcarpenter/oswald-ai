package claude

import (
	"context"
	"strings"
	"testing"
)

func TestStaticModels(t *testing.T) {
	models := StaticModels()
	if len(models) == 0 {
		t.Fatal("no static models")
	}
	for _, m := range models {
		if strings.TrimSpace(m.ID) == "" || m.Window <= 0 {
			t.Fatalf("bad model %+v", m)
		}
	}
}

func TestParseStreamJSON(t *testing.T) {
	out := "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"hello \"}]}}\n{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"world\"}]}}\n{\"type\":\"result\",\"result\":\"final answer\"}\n"
	if got := parseStreamJSON(out); got != "final answer" {
		t.Fatalf("result = %q", got)
	}
	out2 := "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"abc\"}]}}\n"
	if got := parseStreamJSON(out2); got != "abc" {
		t.Fatalf("text = %q", got)
	}
}

func TestCleanEnvDropsKeys(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-key")
	t.Setenv("KEEP_ME", "1")
	env := cleanEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			t.Fatal("credential leaked into child env")
		}
	}
	found := false
	for _, kv := range env {
		if kv == "KEEP_ME=1" {
			found = true
		}
	}
	if !found {
		t.Fatal("unrelated env dropped")
	}
}

func TestCheckFailsWithoutBinary(t *testing.T) {
	old := lookPath
	lookPath = func(string) (string, error) { return "", context.DeadlineExceeded }
	defer func() { lookPath = old }()
	if _, err := Check(context.Background()); err == nil {
		t.Fatal("expected login failure without binary")
	}
}
