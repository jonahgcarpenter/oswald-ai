// Package profiles resolves trusted gateway identities to operator-managed profiles.
package profiles

import (
	"context"
	"errors"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

// LocalOpenAIPrincipal returns the fixed, loopback-only API principal, which
// shares the default profile.
func (d *Directory) LocalOpenAIPrincipal(ctx context.Context) (identity.Principal, error) {
	if err := ctx.Err(); err != nil {
		return identity.Principal{}, err
	}
	return d.Resolve("openai", identity.LocalOpenAIIdentifier, true)
}

// RequiresMention returns shared operator-owned group admission policy.
func (d *Directory) RequiresMention(platform string) bool {
	switch platform {
	case "discord":
		return d.global.DiscordGroupRequireMention
	case "imessage":
		return d.global.BlueBubblesGroupRequireMention
	}
	return true
}

// ErrUnmappedIdentity indicates rejection without creating an account or profile.
var ErrUnmappedIdentity = errors.New("gateway identity has no configured profile")

// Directory is immutable after construction and owns no database account state.
type Directory struct {
	global *config.Config
	routes map[string]string
	log    *config.Logger
}

// NewDirectory validates route targets before any gateways begin accepting work.
func NewDirectory(cfg *config.Config, log *config.Logger) (*Directory, error) {
	if cfg == nil || cfg.ProfileRoot == "" || cfg.ProfileName != "default" {
		return nil, errors.New("invalid global profile configuration")
	}
	directory := &Directory{global: cfg, routes: map[string]string{}, log: log}
	for _, route := range cfg.ProfileRoutes {
		if route.Profile != "default" && cfg.Profiles[route.Profile] == nil {
			return nil, errors.New("undefined profile route target")
		}
		identifier, err := config.NormalizeGatewayIdentifier(route.Platform, route.UserID)
		if err != nil {
			return nil, err
		}
		key := route.Platform + ":" + identifier
		if _, exists := directory.routes[key]; exists {
			return nil, errors.New("duplicate profile route")
		}
		directory.routes[key] = route.Profile
	}
	return directory, nil
}

// Config returns the validated configuration for a known profile, never a path
// derived from client input. Callers must treat returned configuration as immutable.
func (d *Directory) Config(name string) (*config.Config, bool) {
	if d == nil {
		return nil, false
	}
	if name == "default" {
		return d.global, true
	}
	profile, ok := d.global.Profiles[name]
	return profile, ok
}

// Resolve enforces admission policy and returns profile ownership for an
// authenticated transport identity. Banned identities are rejected everywhere;
// a nonempty allow list admits only listed identities in direct messages.
// Unrouted but admitted identities fall back to the default profile. It does
// not create files, users, or DB rows.
func (d *Directory) Resolve(platform, externalID string, direct bool) (_ identity.Principal, resultErr error) {
	started := time.Now()
	defer func() {
		if d != nil && d.log != nil {
			status := "ok"
			if resultErr != nil {
				status = "rejected"
			}
			d.log.Server("profile.routing").Debug("profile.resolve.complete", "resolved gateway profile", config.F("record_kind", "measurement"), config.F("status", status), config.F("duration_ms", time.Since(started).Milliseconds()))
		}
	}()
	if d == nil {
		return identity.Principal{}, ErrUnmappedIdentity
	}
	assurance := identity.Assurance("")
	if platform == "openai" {
		if externalID != identity.LocalOpenAIIdentifier || d.global.OpenAIListenPort == "" {
			return identity.Principal{}, ErrUnmappedIdentity
		}
		// The loopback API shares the default profile.
		return identity.Principal{CanonicalUserID: "default", Gateway: "openai", ExternalID: externalID, Assurance: identity.AssuranceLocalLoopback}, nil
	}
	identifier, err := config.NormalizeGatewayIdentifier(platform, externalID)
	if err != nil {
		return identity.Principal{}, ErrUnmappedIdentity
	}
	switch platform {
	case "discord":
		if d.global.DiscordToken == "" || d.global.DiscordPolicy.Bans(identifier) || (direct && !d.global.DiscordPolicy.Allows(identifier)) {
			return identity.Principal{}, ErrUnmappedIdentity
		}
		assurance = identity.AssuranceDiscordGateway
	case "imessage":
		if d.global.BlueBubblesListenPort == "" || d.global.BlueBubblesPolicy.Bans(identifier) || (direct && !d.global.BlueBubblesPolicy.Allows(identifier)) {
			return identity.Principal{}, ErrUnmappedIdentity
		}
		assurance = identity.AssuranceBlueBubblesWebhook
	default:
		return identity.Principal{}, ErrUnmappedIdentity
	}
	name, ok := d.routes[platform+":"+identifier]
	if !ok {
		// Explicit routes select named profiles; every other admitted identity
		// shares the default profile without creating an account.
		name = "default"
	}
	return identity.Principal{CanonicalUserID: name, Gateway: platform, ExternalID: identifier, Assurance: assurance}, nil
}
