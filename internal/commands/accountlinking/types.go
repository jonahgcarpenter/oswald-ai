package accountlinking

import "github.com/jonahgcarpenter/oswald-ai/internal/database"

// LinkedAccount records a single external gateway identity linked to a canonical user.
type LinkedAccount = database.LinkedAccount

// UserDeletionDescriptor identifies runtime state removed with an account.
type UserDeletionDescriptor struct {
	ExternalIdentities []string
	SessionIDs         []string
}

// DisconnectDescriptor identifies runtime state invalidated by a completed disconnect.
type DisconnectDescriptor struct {
	ExternalIdentities []string
	SessionIDs         []string
}

// UserRecord stores the linked accounts for a canonical Oswald user.
type UserRecord = database.AccountUser

type fileData = database.AccountLinkData

// UserSummary is the command-facing view of a canonical user.
type UserSummary struct {
	CanonicalUserID string
	Intro           string
	Accounts        []LinkedAccount
	IsAdmin         bool
	IsBanned        bool
	BanReason       string
}
