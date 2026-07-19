package accountlinking

import (
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/database"
)

// LinkedAccount records a single external gateway identity linked to a canonical user.
type LinkedAccount = database.LinkedAccount

// ErasureDescriptor identifies runtime state removed by a completed user erasure.
type ErasureDescriptor struct {
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
	CreatedAt       time.Time
	UpdatedAt       time.Time
	IsAdmin         bool
	IsBanned        bool
	BannedBy        string
	BanReason       string
}
