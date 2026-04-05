package accountlink

import "time"

// LinkedAccount records a single external gateway identity linked to a canonical user.
type LinkedAccount struct {
	Gateway     string    `json:"gateway"`
	Identifier  string    `json:"identifier"`
	DisplayName string    `json:"display_name"`
	LinkedAt    time.Time `json:"linked_at"`
	Verified    bool      `json:"verified"`
}

// UserRecord stores the linked accounts for a canonical Oswald user.
type UserRecord struct {
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Accounts  []LinkedAccount `json:"accounts"`
}

type fileData struct {
	Version      int                   `json:"version"`
	Users        map[string]UserRecord `json:"users"`
	AccountIndex map[string]string     `json:"account_index"`
}

// LinkResult describes the outcome of linking an external account.
type LinkResult struct {
	CanonicalUserID string
	AlreadyLinked   bool
	Merged          bool
	LinkedAccount   LinkedAccount
}
