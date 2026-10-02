package profiles

import (
	"errors"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

func TestConfiguredIdentitiesShareProfileWithoutAccounts(t *testing.T) {
	cfg := &config.Config{ProfileRoot: "synthetic", ProfileName: "default", DiscordToken: "synthetic", BlueBubblesListenPort: "1234", OpenAIListenPort: "1235", Profiles: map[string]*config.Config{"alice": {ProfileName: "alice"}, "api": {ProfileName: "api"}}, ProfileRoutes: []config.ProfileRoute{{Platform: "discord", UserID: "123", Profile: "alice"}, {Platform: "imessage", UserID: "+15551234567", Profile: "alice"}}, DiscordPolicy: config.AdmissionPolicy{Mode: "allowlist", AllowFrom: []string{"123"}}, BlueBubblesPolicy: config.AdmissionPolicy{Mode: "allowlist", AllowFrom: []string{"+15551234567"}}}
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
	cfg := &config.Config{ProfileRoot: "synthetic", ProfileName: "default", DiscordToken: "synthetic", Profiles: map[string]*config.Config{"alice": {}}, ProfileRoutes: []config.ProfileRoute{{Platform: "discord", UserID: "123", Profile: "alice"}}, DiscordPolicy: config.AdmissionPolicy{Mode: "allowlist"}}
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
