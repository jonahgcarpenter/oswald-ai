package identity

import "context"

// Resolver maps transport identities to configured ownership. Resolution must
// never provision an application account or grant administrator privileges.
type Resolver interface {
	Resolve(string, string, bool) (Principal, error)
	RequiresMention(string) bool
	LocalOpenAIPrincipal(context.Context) (Principal, error)
}
