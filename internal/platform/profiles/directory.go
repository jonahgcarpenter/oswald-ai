// Package profiles resolves trusted gateway identities to operator-managed profiles.
package profiles

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
)

// LocalOpenAIPrincipal returns the fixed, loopback-only API principal, which
// shares the default profile.
func (d *Directory) LocalOpenAIPrincipal(ctx context.Context) (identity.Principal, error) {
	if err := ctx.Err(); err != nil {
		return identity.Principal{}, err
	}
	return d.Resolve("openai", identity.LocalOpenAIIdentifier, true)
}

// ResolveAPIKey maps a presented bearer key to its owning profile. Keys are
// configured per profile via the `api.key` field and never logged. Empty keys
// never authenticate, and duplicate key material across profiles fails closed
// at directory construction.
func (d *Directory) ResolveAPIKey(rawKey string) (_ identity.Principal, resultErr error) {
	started := time.Now()
	defer func() {
		if d != nil && d.log != nil {
			status := "ok"
			if resultErr != nil {
				status = "rejected"
			}
			d.log.Server("profile.routing").Debug("profile.resolve.complete", "resolved gateway profile", config.F("status", status), config.F("duration_ms", time.Since(started).Milliseconds()))
		}
	}()
	if d == nil || rawKey == "" {
		return identity.Principal{}, ErrUnmappedIdentity
	}
	global := d.get()
	presented := sha256.Sum256([]byte(rawKey))
	candidates := []struct {
		name string
		key  string
	}{{"default", global.OpenAIAPIKey}}
	for name, profile := range global.Profiles {
		candidates = append(candidates, struct {
			name string
			key  string
		}{name, profile.OpenAIAPIKey})
	}
	for _, candidate := range candidates {
		if candidate.key == "" {
			continue
		}
		stored := sha256.Sum256([]byte(candidate.key))
		if subtle.ConstantTimeCompare(presented[:], stored[:]) == 1 {
			return identity.Principal{CanonicalUserID: candidate.name, Gateway: "openai", ExternalID: "api:" + candidate.name, Assurance: identity.AssuranceAPIKey}, nil
		}
	}
	return identity.Principal{}, ErrUnmappedIdentity
}

// RequiresMention returns shared operator-owned group admission policy.
func (d *Directory) RequiresMention(platform string) bool {
	global := d.get()
	if global == nil {
		return true
	}
	switch platform {
	case "discord":
		return global.DiscordGroupRequireMention
	case "imessage":
		return global.BlueBubblesGroupRequireMention
	}
	return true
}

// ErrUnmappedIdentity indicates rejection without creating an account or profile.
var ErrUnmappedIdentity = errors.New("gateway identity has no configured profile")

// Directory resolves trusted gateway identities to operator-managed profiles.
// The live configuration is read per call under a read lock; config reload
// swaps it wholesale via Swap so admission policy and group mention rules take
// effect without a restart. Callers must still treat returned configuration as
// immutable: reload only swaps hot-only diffs, and anything it cannot hot-apply
// stays on the previous revision until a restart.
type Directory struct {
	mutex  sync.RWMutex
	global *config.Config
	routes map[string]string
	log    *config.Logger
}

// get returns the live global configuration.
func (d *Directory) get() *config.Config {
	if d == nil {
		return nil
	}
	d.mutex.RLock()
	defer d.mutex.RUnlock()
	return d.global
}

// Swap replaces the live global configuration after a hot-only reload. The
// candidate must already have validated; routes are rebuilt from it so future
// resolutions use the new mapping. Listeners, model clients, and MCP sessions
// assembled from an earlier revision keep it until a restart.
func (d *Directory) Swap(cfg *config.Config) error {
	if cfg == nil || cfg.ProfileRoot == "" || cfg.ProfileName != "default" {
		return errors.New("invalid global profile configuration")
	}
	routes := map[string]string{}
	for _, route := range cfg.ProfileRoutes {
		key := route.Platform + ":" + route.UserID
		if _, exists := routes[key]; exists {
			return errors.New("duplicate profile route")
		}
		routes[key] = route.Profile
	}
	if d == nil {
		return errors.New("profile directory is unavailable")
	}
	d.mutex.Lock()
	defer d.mutex.Unlock()
	d.global, d.routes = cfg, routes
	return nil
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
	seen := map[[32]byte]string{}
	for _, candidate := range append([]struct {
		name string
		key  string
	}{{"default", cfg.OpenAIAPIKey}}, profileKeys(cfg)...) {
		if candidate.key == "" {
			continue
		}
		sum := sha256.Sum256([]byte(candidate.key))
		if owner, exists := seen[sum]; exists && owner != candidate.name {
			return nil, errors.New("duplicate openai api key")
		}
		seen[sum] = candidate.name
	}
	return directory, nil
}

func profileKeys(cfg *config.Config) []struct {
	name string
	key  string
} {
	out := make([]struct {
		name string
		key  string
	}, 0, len(cfg.Profiles))
	for name, profile := range cfg.Profiles {
		out = append(out, struct {
			name string
			key  string
		}{name, profile.OpenAIAPIKey})
	}
	return out
}

// Config returns the validated configuration for a known profile, never a path
// derived from client input. Callers must treat returned configuration as immutable.
func (d *Directory) Config(name string) (*config.Config, bool) {
	global := d.get()
	if global == nil {
		return nil, false
	}
	if name == "default" {
		return global, true
	}
	profile, ok := global.Profiles[name]
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
			d.log.Server("profile.routing").Debug("profile.resolve.complete", "resolved gateway profile", config.F("status", status), config.F("duration_ms", time.Since(started).Milliseconds()))
		}
	}()
	if d == nil {
		return identity.Principal{}, ErrUnmappedIdentity
	}
	global := d.get()
	if global == nil {
		return identity.Principal{}, ErrUnmappedIdentity
	}
	assurance := identity.Assurance("")
	if platform == "openai" {
		if externalID != identity.LocalOpenAIIdentifier || global.OpenAIListenPort == "" {
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
		if global.DiscordToken == "" || global.DiscordPolicy.Bans(identifier) || (direct && !global.DiscordPolicy.Allows(identifier)) {
			return identity.Principal{}, ErrUnmappedIdentity
		}
		assurance = identity.AssuranceDiscordGateway
	case "imessage":
		if global.BlueBubblesListenPort == "" || global.BlueBubblesPolicy.Bans(identifier) || (direct && !global.BlueBubblesPolicy.Allows(identifier)) {
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
