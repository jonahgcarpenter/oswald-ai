package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const classifyFixture = `providers:
  fake:
    api: http://127.0.0.1:9999/v1
    key: test-key
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
platforms:
  api:
    enabled: true
    extra:
      api_host: 127.0.0.1
      api_port: 12345
runtime:
  log_level: info
`

func classifyRoot(t *testing.T, text string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCommitRejectsInvalidCandidate(t *testing.T) {
	root := classifyRoot(t, classifyFixture)
	path := filepath.Join(root, "config.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := []byte("platforms: []\n")
	if err := CommitConfigCandidate(root, path, bad, before); err == nil {
		t.Fatal("invalid candidate accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("file mutated on failed validation")
	}
	if _, err := os.Stat(LastKnownGoodPath(path)); !os.IsNotExist(err) {
		t.Fatal("LKG written on failed validation")
	}
}

func TestCommitWritesLKGOnSuccess(t *testing.T) {
	root := classifyRoot(t, classifyFixture)
	path := filepath.Join(root, "config.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	good := []byte(classifyFixture + "# touch\n")
	// Trailing comment keeps the document valid while changing bytes.
	candidate := append([]byte(classifyFixture), []byte("# touch\n")...)
	_ = good
	if err := CommitConfigCandidate(root, path, candidate, before); err != nil {
		t.Fatalf("valid commit rejected: %v", err)
	}
	lkg, err := ReadLastKnownGood(path)
	if err != nil {
		t.Fatalf("LKG missing: %v", err)
	}
	if string(lkg) != string(before) {
		t.Fatal("LKG does not hold previous bytes")
	}
}

func TestClassifyReloadHotAndRestart(t *testing.T) {
	base, err := Validate(classifyRoot(t, classifyFixture))
	if err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	hot, classes := ClassifyReload(base, base)
	if !hot || len(classes) != 0 {
		t.Fatalf("identical = hot=%v classes=%v, want hot no classes", hot, classes)
	}
	if got := ReloadEffect(base, base); got != "live" {
		t.Fatalf("identical effect = %q, want live", got)
	}
	changed, err := Validate(classifyRoot(t, classifyFixture+"mcp_servers:\n  notes:\n    url: https://example.com/mcp\n"))
	if err != nil {
		t.Fatalf("variant invalid: %v", err)
	}
	hot, classes = ClassifyReload(base, changed)
	if hot {
		t.Fatal("mcp change reported hot, want restart")
	}
	found := false
	for _, c := range classes {
		if c == "mcp_servers" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mcp_servers class missing in %v", classes)
	}
	if got := ReloadEffect(base, changed); got != "restart" {
		t.Fatalf("mcp effect = %q, want restart", got)
	}
}

const classifyRoutingBase = `providers:
  fake:
    api: http://127.0.0.1:9999/v1
    key: test-key
  other:
    api: http://127.0.0.1:9998/v1
    key: other-key
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
gateway:
  multiplex_profiles: true
  profile_routes:
    - platform: discord
      user_id: "555555555555555555"
      profile: alice
platforms:
  api:
    enabled: true
    extra:
      api_host: 127.0.0.1
      api_port: 12345
runtime:
  log_level: info
`

const classifyRoutingProfile = `model:
  default: fake/alice
`

// classifyRoutingConfig validates a default config plus one named profile so
// routing-field and window changes on profiles present in both maps are
// exercised through the real loader. Both configs come from the same root, as
// a reload does, so profile paths stay identical.
func classifyRoutingConfig(t *testing.T, defaultText, profileText string) *Config {
	t.Helper()
	_, next := classifyRoutingPair(t, defaultText, profileText, defaultText, profileText)
	return next
}

func classifyRoutingPair(t *testing.T, oldDefault, oldProfile, nextDefault, nextProfile string) (*Config, *Config) {
	t.Helper()
	root := classifyRoot(t, oldDefault)
	profileRoot := filepath.Join(root, "profiles", "alice")
	if err := os.MkdirAll(profileRoot, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(defaultText, profileText string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(defaultText), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(profileRoot, "config.yaml"), []byte(profileText), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(oldDefault, oldProfile)
	oldCfg, err := Validate(root)
	if err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	write(nextDefault, nextProfile)
	nextCfg, err := Validate(root)
	if err != nil {
		t.Fatalf("variant invalid: %v", err)
	}
	return oldCfg, nextCfg
}

func TestClassifyReloadModelRouting(t *testing.T) {
	for _, tc := range []struct {
		name        string
		defaultText string
		profileText string
		hot         bool
		classes     []string
	}{
		{"unchanged", classifyRoutingBase, classifyRoutingProfile, true, nil},
		{
			"default model only",
			strings.Replace(classifyRoutingBase, "default: fake/model", "default: fake/other", 1),
			classifyRoutingProfile,
			true,
			[]string{"model"},
		},
		{
			"default provider switch",
			strings.Replace(classifyRoutingBase, "provider: custom:fake", "provider: custom:other", 1),
			classifyRoutingProfile,
			true,
			[]string{"providers", "model"},
		},
		{
			"default context window only",
			strings.Replace(classifyRoutingBase, "context_length: 32768", "context_length: 65536", 1),
			classifyRoutingProfile,
			false,
			[]string{"model_window"},
		},
		{
			"default model and context window",
			strings.Replace(strings.Replace(classifyRoutingBase, "default: fake/model", "default: fake/other", 1), "context_length: 32768", "context_length: 65536", 1),
			classifyRoutingProfile,
			false,
			[]string{"model", "model_window"},
		},
		{
			"named profile model only",
			classifyRoutingBase,
			strings.Replace(classifyRoutingProfile, "default: fake/alice", "default: fake/alice-next", 1),
			true,
			[]string{"model"},
		},
		{
			"named profile context window only",
			classifyRoutingBase,
			classifyRoutingProfile + "  context_length: 16384\n",
			false,
			[]string{"model_window"},
		},
		{
			"named profile model and mcp server",
			classifyRoutingBase,
			"model:\n  default: fake/alice-next\nmcp_servers:\n  notes:\n    url: https://example.com/mcp\n",
			false,
			[]string{"model", "profiles"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, next := classifyRoutingPair(t, classifyRoutingBase, classifyRoutingProfile, tc.defaultText, tc.profileText)
			hot, classes := ClassifyReload(base, next)
			if hot != tc.hot {
				t.Fatalf("hot=%v want %v (classes=%v)", hot, tc.hot, classes)
			}
			if !slices.Equal(classes, tc.classes) {
				t.Fatalf("classes=%v want %v", classes, tc.classes)
			}
			wantEffect := "live"
			if !tc.hot {
				wantEffect = "restart"
			}
			if got := ReloadEffect(base, next); got != wantEffect {
				t.Fatalf("effect=%q want %q", got, wantEffect)
			}
		})
	}
}

const classifyOverrideBase = `providers:
  fake:
    api: http://127.0.0.1:9999/v1
    key: test-key
  other:
    api: http://127.0.0.1:9998/v1
    key: other-key
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
gateway:
  multiplex_profiles: true
  profile_routes:
    - platform: discord
      user_id: "555555555555555555"
      profile: alice
platforms:
  discord:
    enabled: true
    extra:
      token: test-token
    channel_overrides:
      "555000111":
        provider: custom:other
        model: other/model
runtime:
  log_level: info
`

const classifyOverrideNoProfile = `providers:
  fake:
    api: http://127.0.0.1:9999/v1
    key: test-key
  other:
    api: http://127.0.0.1:9998/v1
    key: other-key
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
platforms:
  discord:
    enabled: true
    extra:
      token: test-token
    channel_overrides:
      "555000111":
        provider: custom:other
        model: other/model
runtime:
  log_level: info
`

func TestClassifyReloadChannelOverrides(t *testing.T) {
	const profile = "model:\n  default: fake/alice\n"
	for _, tc := range []struct {
		name        string
		defaultText string
		hot         bool
		classes     []string
	}{
		{"unchanged", classifyOverrideBase, true, nil},
		{
			"override only",
			strings.Replace(classifyOverrideBase, "model: other/model", "model: other/next", 1),
			true,
			[]string{"channel_overrides"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, next := classifyRoutingPair(t, classifyOverrideBase, profile, tc.defaultText, profile)
			if alice := base.Profiles["alice"]; alice == nil || !reflect.DeepEqual(alice.ChannelOverrides, base.ChannelOverrides) {
				t.Fatalf("fixture missing inherited overrides: %+v", alice)
			}
			hot, classes := ClassifyReload(base, next)
			if hot != tc.hot {
				t.Fatalf("hot=%v want %v (classes=%v)", hot, tc.hot, classes)
			}
			if !slices.Equal(classes, tc.classes) {
				t.Fatalf("classes=%v want %v", classes, tc.classes)
			}
		})
	}
}

const classifyProfileOverride = `model:
  default: fake/alice
platforms:
  discord:
    channel_overrides:
      "555000222":
        provider: custom:other
        model: other/alice
`

func TestClassifyReloadProfileChannelOverrides(t *testing.T) {
	// A profile-only overlay edit leaves the root map identical but must stay
	// hot as channel_overrides rather than collapsing into profiles/restart.
	nextProfile := strings.Replace(classifyProfileOverride, "model: other/alice", "model: other/alice-next", 1)
	base, next := classifyRoutingPair(t, classifyOverrideBase, classifyProfileOverride, classifyOverrideBase, nextProfile)
	if !reflect.DeepEqual(base.ChannelOverrides, next.ChannelOverrides) {
		t.Fatal("fixture root override map moved")
	}
	if reflect.DeepEqual(base.Profiles["alice"].ChannelOverrides, next.Profiles["alice"].ChannelOverrides) {
		t.Fatal("fixture profile override map did not move")
	}
	hot, classes := ClassifyReload(base, next)
	if !hot || !slices.Equal(classes, []string{"channel_overrides"}) {
		t.Fatalf("profile override diff = hot=%v classes=%v, want hot [channel_overrides]", hot, classes)
	}

	// A profile window edit alongside the overlay still bills both classes.
	windowProfile := strings.Replace(classifyProfileOverride, "model: other/alice", "model: other/alice-next", 1)
	windowProfile = strings.Replace(windowProfile, "  default: fake/alice\n", "  default: fake/alice\n  context_length: 16384\n", 1)
	windowBase, windowNext := classifyRoutingPair(t, classifyOverrideBase, classifyProfileOverride, classifyOverrideBase, windowProfile)
	hot, classes = ClassifyReload(windowBase, windowNext)
	if hot || !slices.Equal(classes, []string{"model_window", "channel_overrides"}) {
		t.Fatalf("profile override+window diff = hot=%v classes=%v, want restart [model_window channel_overrides]", hot, classes)
	}
}

func TestClassifyReloadProfileLifecycleWithOverrides(t *testing.T) {
	base := classifyRoutingConfig(t, classifyOverrideBase, classifyProfileOverride)
	alice := base.Profiles["alice"]
	if alice == nil {
		t.Fatal("fixture missing alice profile")
	}
	added := *base
	bob := *alice
	bob.ProfileName = "bob"
	bob.ChannelOverrides = map[string]map[string]ChannelOverride{
		"discord": {"555000333": {Provider: "other", Model: "other/bob"}},
	}
	added.Profiles = map[string]*Config{"alice": alice, "bob": &bob}
	hot, classes := ClassifyReload(base, &added)
	if hot || !slices.Equal(classes, []string{"profiles"}) {
		t.Fatalf("added profile with overrides = hot=%v classes=%v, want restart [profiles]", hot, classes)
	}
	removed := *base
	removed.Profiles = map[string]*Config{}
	hot, classes = ClassifyReload(base, &removed)
	if hot || !slices.Equal(classes, []string{"profiles"}) {
		t.Fatalf("removed profile with overrides = hot=%v classes=%v, want restart [profiles]", hot, classes)
	}
}

func TestClassifyReloadChannelOverridesWithPlatformRestart(t *testing.T) {
	base, err := Validate(classifyRoot(t, classifyOverrideNoProfile))
	if err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	nextText := strings.Replace(classifyOverrideNoProfile, "token: test-token", "token: other-token", 1)
	nextText = strings.Replace(nextText, "model: other/model", "model: other/next", 1)
	next, err := Validate(classifyRoot(t, nextText))
	if err != nil {
		t.Fatalf("variant invalid: %v", err)
	}
	hot, classes := ClassifyReload(base, next)
	if hot || !slices.Equal(classes, []string{"platforms", "channel_overrides"}) {
		t.Fatalf("override+token = hot=%v classes=%v, want restart [platforms channel_overrides]", hot, classes)
	}
}

func TestClassifyReloadNamedProfileLifecycle(t *testing.T) {
	base := classifyRoutingConfig(t, classifyRoutingBase, classifyRoutingProfile)
	alice := base.Profiles["alice"]
	if alice == nil {
		t.Fatal("fixture missing alice profile")
	}
	added := *base
	bob := *alice
	bob.ProfileName = "bob"
	added.Profiles = map[string]*Config{"alice": alice, "bob": &bob}
	hot, classes := ClassifyReload(base, &added)
	if hot || !slices.Equal(classes, []string{"profiles"}) {
		t.Fatalf("added profile = hot=%v classes=%v, want restart [profiles]", hot, classes)
	}
	removed := *base
	removed.Profiles = map[string]*Config{}
	hot, classes = ClassifyReload(base, &removed)
	if hot || !slices.Equal(classes, []string{"profiles"}) {
		t.Fatalf("removed profile = hot=%v classes=%v, want restart [profiles]", hot, classes)
	}
}

func TestClassifyReloadHotRuntimeAndRestartAPI(t *testing.T) {
	base, err := Validate(classifyRoot(t, classifyFixture))
	if err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	runtimeNext, err := Validate(classifyRoot(t, strings.Replace(classifyFixture, "log_level: info", "log_level: debug", 1)))
	if err != nil {
		t.Fatalf("runtime variant invalid: %v", err)
	}
	hot, classes := ClassifyReload(base, runtimeNext)
	if !hot || !slices.Equal(classes, []string{"runtime"}) {
		t.Fatalf("runtime diff = hot=%v classes=%v, want hot [runtime]", hot, classes)
	}
	apiNext, err := Validate(classifyRoot(t, strings.Replace(classifyFixture, "api_port: 12345", "api_port: 12346", 1)))
	if err != nil {
		t.Fatalf("api variant invalid: %v", err)
	}
	hot, classes = ClassifyReload(base, apiNext)
	if hot || !slices.Equal(classes, []string{"api"}) {
		t.Fatalf("api diff = hot=%v classes=%v, want restart [api]", hot, classes)
	}
}
