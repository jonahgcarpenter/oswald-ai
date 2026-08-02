package database

// AccountUser stores the linked accounts for a canonical Oswald user.
type AccountUser struct {
	IsAdmin   bool            `json:"is_admin"`
	IsBanned  bool            `json:"is_banned"`
	BanReason string          `json:"ban_reason"`
	Accounts  []LinkedAccount `json:"accounts"`
}
