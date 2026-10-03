package profiles

import (
	"errors"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

func TestConfiguredIdentitiesShareProfileWithoutAccounts(t *testing.T) {
	cfg := &config.Config{ProfileRoot: "synthetic", ProfileName: "default", DiscordToken: "synthetic", BlueBubblesListenPort: "1234", OpenAIListenPort: "1235", Profiles: map[string]*config.Config{"alice": {ProfileName: "alice"}, "api": {ProfileName: "api"}}, ProfileRoutes: []config.ProfileRoute{{Platform: "discord", UserID: "123", Profile: "alice"}, {Platform: "imessage", UserID: "+15551234567", Profile: "alice"}}, DiscordPolicy: config.AdmissionPolicy{Allowed: []string{"123"}}, BlueBubblesPolicy: config.AdmissionPolicy{Allowed: []string{"+15551234567"}}}
	directory, err := NewDirectory(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ platform, id string }{{"discord", "123"}, {"imessage", "+1 (555) 123-4567"}} {
		principal, err := directory.Resolve(tc.platform, tc.id, true)
		if err != nil || !principal.Authenticated() || principal.CanonicalUserID != "alice" {
			t.Fatal("incorrect profile resolution", err)
		}
	}
	principal, err := directory.Resolve("openai", identity.LocalOpenAIIdentifier, true)
	if err != nil || principal.CanonicalUserID != "api" || !principal.Authenticated() {
		t.Fatal("incorrect API ownership", err)
	}
	if _, err := directory.Resolve("discord", "456", true); !errors.Is(err, ErrUnmappedIdentity) {
		t.Fatal("unmapped identity admitted")
	}
	if _, ok := directory.Config("../alice"); ok {
		t.Fatal("unsafe profile selected")
	}
}

func TestProfileAdmissionIsNotAnAdministratorGrant(t *testing.T) {
	cfg := &config.Config{ProfileRoot: "synthetic", ProfileName: "default", DiscordToken: "synthetic", Profiles: map[string]*config.Config{"alice": {}}, ProfileRoutes: []config.ProfileRoute{{Platform: "discord", UserID: "123", Profile: "alice"}}, DiscordPolicy: config.AdmissionPolicy{Allowed: []string{"999"}}}
	directory, err := NewDirectory(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Resolve("discord", "123", true); !errors.Is(err, ErrUnmappedIdentity) {
		t.Fatal("DM allowlist ignored")
	}
	if _, err := directory.Resolve("discord", "123", false); err != nil {
		t.Fatal("configured group identity rejected", err)
	}
}

func TestAdmittedIdentityWithoutRouteFallsBackToDefault(t *testing.T) {
	cfg := &config.Config{ProfileRoot: "synthetic", ProfileName: "default", DiscordToken: "synthetic", BlueBubblesListenPort: "1234", Profiles: map[string]*config.Config{"alice": {ProfileName: "alice"}}, ProfileRoutes: []config.ProfileRoute{{Platform: "discord", UserID: "123", Profile: "alice"}}, DiscordPolicy: config.AdmissionPolicy{}, BlueBubblesPolicy: config.AdmissionPolicy{}}
	directory, err := NewDirectory(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, platform, id string
		direct             bool
	}{
		{"unrouted discord DM", "discord", "456", true},
		{"unrouted discord group", "discord", "456", false},
		{"unrouted imessage DM", "imessage", "+15557654321", true},
		{"unrouted imessage group", "imessage", "+15557654321", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			principal, err := directory.Resolve(tc.platform, tc.id, tc.direct)
			if err != nil || principal.CanonicalUserID != "default" || !principal.Authenticated() {
				t.Fatalf("unrouted identity did not fall back to default: %+v err=%v", principal, err)
			}
		})
	}
	// A named route still wins, and a ban applies everywhere.
	principal, err := directory.Resolve("discord", "123", true)
	if err != nil || principal.CanonicalUserID != "alice" {
		t.Fatalf("named route lost: %+v err=%v", principal, err)
	}
	banned := &config.Config{ProfileRoot: "synthetic", ProfileName: "default", DiscordToken: "synthetic", Profiles: map[string]*config.Config{}, ProfileRoutes: nil, DiscordPolicy: config.AdmissionPolicy{Banned: []string{"456"}}}
	bannedDirectory, err := NewDirectory(banned, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bannedDirectory.Resolve("discord", "456", true); !errors.Is(err, ErrUnmappedIdentity) {
		t.Fatal("banned identity admitted in DM")
	}
	if _, err := bannedDirectory.Resolve("discord", "456", false); !errors.Is(err, ErrUnmappedIdentity) {
		t.Fatal("banned identity admitted in group")
	}
}
