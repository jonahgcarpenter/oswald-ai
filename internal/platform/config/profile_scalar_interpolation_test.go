package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfileInterpolationRetainsScalarTypes(t *testing.T) {
	text := strings.Replace(syntheticProfileConfig, "context_length: 32768", `context_length: "${OSWALD_TEST_CONTEXT}"`, 1)
	text = strings.Replace(text, "enabled: true", `enabled: "${OSWALD_TEST_ENABLED}"`, 1)
	text = strings.Replace(text, "api_port: 12345", `api_port: "${OSWALD_TEST_PORT}"`, 1)
	root := profileConfigFixture(t, text)
	t.Setenv("OSWALD_TEST_CONTEXT", "65536")
	t.Setenv("OSWALD_TEST_ENABLED", "true")
	t.Setenv("OSWALD_TEST_PORT", "12345")
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelContextWindow != 65536 || cfg.Profiles["alice"].ModelContextWindow != 65536 || cfg.OpenAIListenPort != "12345" {
		t.Fatal("interpolated numeric settings did not retain their types")
	}
}

func TestEmptyProcessCredentialOverridesDotenv(t *testing.T) {
	root := profileConfigFixture(t, strings.Replace(syntheticProfileConfig, "key: \"${PROFILE_TEST_MODEL_KEY}\"", `key: "${OSWALD_TEST_EMPTY_OVERRIDE}"`, 1))
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("OSWALD_TEST_EMPTY_OVERRIDE=private-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSWALD_TEST_EMPTY_OVERRIDE", "")
	if _, err := LoadProfiles(root); err == nil {
		t.Fatal("empty process credential fell back to dotenv")
	}
}
