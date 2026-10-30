package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateAgreesWithLoadProfiles(t *testing.T) {
	t.Setenv("PROFILE_TEST_MODEL_KEY", "synthetic-process-credential")
	cases := []struct {
		name    string
		mutate  func(text string) string
		wantErr bool
	}{
		{"valid", func(text string) string { return text }, false},
		{"bad type", func(text string) string {
			return strings.Replace(text, "context_length: 32768", "context_length: not-a-number", 1)
		}, true},
		{"bad interpolation", func(text string) string {
			return strings.Replace(text, `"${PROFILE_TEST_MODEL_KEY}"`, `"${UNSET"`, 1)
		}, true},
		{"missing required var", func(text string) string {
			return strings.Replace(text, `"${PROFILE_TEST_MODEL_KEY}"`, `"prefix-${OSWALD_TEST_DEFINITELY_MISSING}-suffix"`, 1)
		}, true},
		{"bad webhook path", func(text string) string {
			return text + "platforms:\n  bluebubbles:\n    enabled: true\n    extra:\n      webhook_path: \"..\\u002f..\\u002fetc\"\n"
		}, true},
		{"bad profile name", func(text string) string {
			return strings.Replace(text, "profile: alice", "profile: ../outside", 1)
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			build := func() string {
				root := profileConfigFixture(t, tc.mutate(syntheticProfileConfig))
				if err := os.MkdirAll(filepath.Join(root, "profiles", "alice"), 0700); err != nil {
					t.Fatal(err)
				}
				return root
			}
			_, loadErr := LoadProfiles(build())
			_, validateErr := Validate(build())
			if (loadErr != nil) != (validateErr != nil) {
				t.Fatalf("LoadProfiles err=%v Validate err=%v: disagree", loadErr, validateErr)
			}
			if !tc.wantErr && (loadErr != nil || validateErr != nil) {
				t.Fatalf("valid fixture rejected: load=%v validate=%v", loadErr, validateErr)
			}
			if tc.wantErr && loadErr == nil {
				t.Fatal("invalid fixture accepted")
			}
			if loadErr != nil && validateErr != nil {
				loadCode, loadOK := AsConfigError(loadErr)
				validateCode, validateOK := AsConfigError(validateErr)
				if !loadOK || !validateOK {
					t.Fatalf("errors not classified: load=%v validate=%v", loadErr, validateErr)
				}
				if loadCode.Code != validateCode.Code || loadCode.Path != validateCode.Path {
					t.Fatalf("code/path disagree: load=%+v validate=%+v", loadCode, validateCode)
				}
			}
		})
	}
}

func TestValidateHasNoSideEffects(t *testing.T) {
	t.Setenv("PROFILE_TEST_MODEL_KEY", "synthetic-process-credential")
	root := profileConfigFixture(t, syntheticProfileConfig)
	if err := os.Chmod(filepath.Join(root, "config.yaml"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(root); err == nil {
		t.Fatal("strict validation accepted loose permissions")
	}
	if info, err := os.Stat(filepath.Join(root, "config.yaml")); err != nil || info.Mode().Perm() != 0644 {
		t.Fatal("Validate repaired live state")
	}
}
