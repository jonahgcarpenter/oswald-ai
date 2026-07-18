// Package identity defines request identity independently from gateways,
// account persistence, and authorization policy.
package identity

// Assurance describes how a gateway established an external identity.
type Assurance string

const (
	// AssuranceSelfAsserted identifies an unverified identity supplied by a client.
	AssuranceSelfAsserted Assurance = "self_asserted"
	// AssuranceDiscordGateway identifies a Discord user asserted by Discord's gateway.
	AssuranceDiscordGateway Assurance = "discord_gateway"
	// AssuranceBlueBubblesWebhook identifies an iMessage sender asserted by an
	// authenticated BlueBubbles webhook.
	AssuranceBlueBubblesWebhook Assurance = "bluebubbles_webhook"
)

// Principal is the resolved actor for one request. CanonicalUserID is the only
// field used for tenant ownership; ExternalID remains transport-facing identity.
type Principal struct {
	CanonicalUserID string
	Gateway         string
	ExternalID      string
	Assurance       Assurance
}

// Valid reports whether the principal contains a resolved canonical identity
// and a recognized identity assurance source.
func (p Principal) Valid() bool {
	if p.CanonicalUserID == "" || p.Gateway == "" || p.ExternalID == "" {
		return false
	}
	switch p.Gateway {
	case "websocket":
		return p.Assurance == AssuranceSelfAsserted
	case "discord":
		return p.Assurance == AssuranceDiscordGateway
	case "imessage":
		return p.Assurance == AssuranceBlueBubblesWebhook
	}
	return false
}

// Authenticated reports whether a trusted gateway established the external
// identity. It is independent from account-link verification and authorization.
func (p Principal) Authenticated() bool {
	if !p.Valid() {
		return false
	}
	switch p.Assurance {
	case AssuranceDiscordGateway, AssuranceBlueBubblesWebhook:
		return true
	default:
		return false
	}
}
