package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetModelSelectionCommitsBothKeysAtomically(t *testing.T) {
	root := classifyRoot(t, classifyFixture)
	path := filepath.Join(root, "config.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := SetModelSelection(root, "default", "fake", "fake/next-model"); err != nil {
		t.Fatalf("set model selection: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "provider: custom:fake") || !strings.Contains(string(after), "default: fake/next-model") {
		t.Fatalf("candidate missing both keys:\n%s", after)
	}
	cfg, err := Validate(root)
	if err != nil {
		t.Fatalf("committed candidate invalid: %v", err)
	}
	if cfg.LLMGatewayProvider != "fake" || cfg.LLMGatewayModel != "fake/next-model" {
		t.Fatalf("effective selection = %q/%q", cfg.LLMGatewayProvider, cfg.LLMGatewayModel)
	}
	lkg, err := ReadLastKnownGood(path)
	if err != nil {
		t.Fatalf("last-known-good missing: %v", err)
	}
	if string(lkg) != string(before) {
		t.Fatal("last-known-good does not hold the previous bytes")
	}
}

func TestSetModelSelectionUndeclaredProviderLeavesFileIdentical(t *testing.T) {
	root := classifyRoot(t, classifyFixture)
	path := filepath.Join(root, "config.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	err = SetModelSelection(root, "default", "missing", "missing/model")
	if err == nil {
		t.Fatal("undeclared provider accepted")
	}
	if code := ErrorCode(err); code != "config_model_provider_invalid" {
		t.Fatalf("error code = %q, want config_model_provider_invalid", code)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(before) {
		t.Fatal("file mutated on failed validation")
	}
	if _, statErr := os.Stat(LastKnownGoodPath(path)); !os.IsNotExist(statErr) {
		t.Fatal("last-known-good written on failed validation")
	}
}

func TestSetModelSelectionRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		model    string
	}{
		{name: "empty provider", provider: "", model: "fake/model"},
		{name: "oversize provider", provider: strings.Repeat("p", modelSelectionValueRuneLimit+1), model: "fake/model"},
		{name: "invalid UTF-8 provider", provider: "bad\xff", model: "fake/model"},
		{name: "empty model", provider: "fake", model: ""},
		{name: "oversize model", provider: "fake", model: strings.Repeat("m", modelSelectionValueRuneLimit+1)},
		{name: "invalid UTF-8 model", provider: "fake", model: "bad\xff"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := classifyRoot(t, classifyFixture)
			path := filepath.Join(root, "config.yaml")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			err = SetModelSelection(root, "default", test.provider, test.model)
			if err == nil {
				t.Fatal("invalid value accepted")
			}
			if _, ok := AsConfigError(err); !ok {
				t.Fatalf("error is not classified: %v", err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(after) != string(before) {
				t.Fatal("file mutated on rejected input")
			}
		})
	}
}

func TestSetModelSelectionNamedProfilePath(t *testing.T) {
	root := profileConfigFixture(t, syntheticProfileConfig)
	profilePath := filepath.Join(root, "profiles", "alice", "config.yaml")
	defaultPath := filepath.Join(root, "config.yaml")
	defaultBefore, err := os.ReadFile(defaultPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := SetModelSelection(root, "alice", "fake", "fake/alice-next"); err != nil {
		t.Fatalf("set named profile selection: %v", err)
	}

	profileData, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("profile config not written: %v", err)
	}
	if !strings.Contains(string(profileData), "provider: custom:fake") || !strings.Contains(string(profileData), "default: fake/alice-next") {
		t.Fatalf("profile candidate missing both keys:\n%s", profileData)
	}
	cfg, err := Validate(root)
	if err != nil {
		t.Fatalf("committed named profile invalid: %v", err)
	}
	alice := cfg.Profiles["alice"]
	if alice == nil {
		t.Fatal("named profile missing from validated config")
	}
	if alice.LLMGatewayProvider != "fake" || alice.LLMGatewayModel != "fake/alice-next" {
		t.Fatalf("named profile selection = %q/%q", alice.LLMGatewayProvider, alice.LLMGatewayModel)
	}
	if cfg.LLMGatewayProvider != "fake" || cfg.LLMGatewayModel != "fake/model" {
		t.Fatalf("default profile changed = %q/%q", cfg.LLMGatewayProvider, cfg.LLMGatewayModel)
	}
	defaultAfter, err := os.ReadFile(defaultPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(defaultAfter) != string(defaultBefore) {
		t.Fatal("named profile write touched the global config")
	}
}
