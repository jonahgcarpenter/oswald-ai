package accountlinking

import "github.com/jonahgcarpenter/oswald-ai/internal/database"

// LinkedAccount records a single external gateway identity linked to a canonical user.
type LinkedAccount = database.LinkedAccount

// UserRecord stores the linked accounts for a canonical Oswald user.
type UserRecord = database.AccountUser

type fileData = database.AccountLinkData

// LinkResult describes the outcome of linking an external account.
type LinkResult struct {
	CanonicalUserID string
	AlreadyLinked   bool
	Merged          bool
	LinkedAccount   LinkedAccount
}
