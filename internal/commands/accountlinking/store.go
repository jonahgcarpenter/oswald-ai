package accountlinking

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

// Service manages canonical user IDs and linked gateway accounts.
type Service struct {
	path       string
	legacyPath string
	memories   *usermemory.Store
	log        *config.Logger
	db         *database.DB
	mu         sync.Mutex
	initOnce   sync.Once
	initErr    error
}

// NewService creates a new account-link service backed by a SQLite database on disk.
func NewService(path string, memories *usermemory.Store, log *config.Logger) *Service {
	legacyPath := filepath.Join(filepath.Dir(path), "links.json")
	if path == config.DefaultAccountLinkPath {
		legacyPath = config.DefaultLegacyAccountLinkPath
	}
	return &Service{path: path, legacyPath: legacyPath, memories: memories, log: log}
}

// Initialize prepares the account-link database and migrates the legacy JSON store when present.
func (s *Service) Initialize() error {
	s.initOnce.Do(func() {
		s.initErr = s.initialize()
	})
	return s.initErr
}

// EnsureAccount resolves an external account to a canonical user ID, creating one when needed.
func (s *Service) EnsureAccount(gateway, identifier, displayName string) (string, error) {
	identifier, err := NormalizeIdentifier(gateway, identifier)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return "", err
	}

	key := accountKey(gateway, identifier)
	if canonicalID, ok := data.AccountIndex[key]; ok {
		user := data.Users[canonicalID]
		updated := false
		for i := range user.Accounts {
			if user.Accounts[i].Gateway == gateway && user.Accounts[i].Identifier == identifier {
				if displayName != "" && user.Accounts[i].DisplayName != displayName {
					user.Accounts[i].DisplayName = displayName
					updated = true
				}
				break
			}
		}
		if updated {
			user.UpdatedAt = time.Now().UTC()
			data.Users[canonicalID] = user
			if err := s.saveLocked(data); err != nil {
				return "", err
			}
		}
		if err := s.memories.SyncSpeakerIntro(canonicalID, FormatSpeakerLine(user.Accounts)); err != nil {
			return "", err
		}
		return canonicalID, nil
	}

	now := time.Now().UTC()
	canonicalID, err := newCanonicalUserID()
	if err != nil {
		return "", err
	}
	data.Users[canonicalID] = UserRecord{
		CreatedAt: now,
		UpdatedAt: now,
		Accounts: []LinkedAccount{{
			Gateway:     strings.ToLower(gateway),
			Identifier:  identifier,
			DisplayName: displayName,
			LinkedAt:    now,
			Verified:    false,
		}},
	}
	data.AccountIndex[key] = canonicalID

	if err := s.saveLocked(data); err != nil {
		return "", err
	}
	if err := s.memories.SyncSpeakerIntro(canonicalID, FormatSpeakerLine(data.Users[canonicalID].Accounts)); err != nil {
		return "", err
	}

	s.log.Debug("account_link.canonical_user.created", "created canonical user", config.F("target_user_id", canonicalID), config.F("account", key))
	return canonicalID, nil
}

// AccountsForUser returns the linked accounts for a canonical user.
func (s *Service) AccountsForUser(canonicalUserID string) ([]LinkedAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return nil, err
	}

	user, ok := data.Users[canonicalUserID]
	if !ok {
		return nil, nil
	}

	accounts := append([]LinkedAccount(nil), user.Accounts...)
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Gateway == accounts[j].Gateway {
			return accounts[i].Identifier < accounts[j].Identifier
		}
		return accounts[i].Gateway < accounts[j].Gateway
	})
	return accounts, nil
}

// ListUsers returns all canonical users with compact account and status details.
func (s *Service) ListUsers() ([]UserSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return nil, err
	}

	userIDs := make([]string, 0, len(data.Users))
	for canonicalID := range data.Users {
		userIDs = append(userIDs, canonicalID)
	}
	sort.Strings(userIDs)

	users := make([]UserSummary, 0, len(userIDs))
	for _, canonicalID := range userIDs {
		users = append(users, summarizeUser(canonicalID, data.Users[canonicalID]))
	}
	return users, nil
}

// User returns one canonical user's summary.
func (s *Service) User(canonicalUserID string) (UserSummary, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return UserSummary{}, false, err
	}
	user, ok := data.Users[canonicalUserID]
	if !ok {
		return UserSummary{}, false, nil
	}
	return summarizeUser(canonicalUserID, user), true, nil
}

// IsAdmin reports whether a canonical user can run admin commands.
func (s *Service) IsAdmin(canonicalUserID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return false, err
	}
	user, ok := data.Users[canonicalUserID]
	return ok && user.IsAdmin, nil
}

// IsBanned reports whether a canonical user is blocked from using Oswald.
func (s *Service) IsBanned(canonicalUserID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return false, err
	}
	user, ok := data.Users[canonicalUserID]
	return ok && user.IsBanned, nil
}

// BanStatus returns whether a canonical user is banned and the stored reason.
func (s *Service) BanStatus(canonicalUserID string) (bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return false, "", err
	}
	user, ok := data.Users[canonicalUserID]
	if !ok || !user.IsBanned {
		return false, "", nil
	}
	return true, user.BanReason, nil
}

// SetAdmin updates a canonical user's admin flag.
func (s *Service) SetAdmin(actorID, targetID string, isAdmin bool) error {
	if actorID == targetID && !isAdmin {
		return fmt.Errorf("cannot remove admin from yourself")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return err
	}
	user, ok := data.Users[targetID]
	if !ok {
		return fmt.Errorf("canonical user %q not found", targetID)
	}
	if user.IsAdmin == isAdmin {
		return nil
	}
	user.IsAdmin = isAdmin
	user.UpdatedAt = time.Now().UTC()
	data.Users[targetID] = user
	return s.saveLocked(data)
}

// BanUser marks a canonical user as banned.
func (s *Service) BanUser(actorID, targetID, reason string) error {
	if actorID == targetID {
		return fmt.Errorf("cannot ban yourself")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return err
	}
	user, ok := data.Users[targetID]
	if !ok {
		return fmt.Errorf("canonical user %q not found", targetID)
	}
	now := time.Now().UTC()
	user.IsBanned = true
	user.BannedAt = now
	user.BannedBy = actorID
	user.BanReason = strings.TrimSpace(reason)
	user.UpdatedAt = now
	data.Users[targetID] = user
	return s.saveLocked(data)
}

// UnbanUser clears a canonical user's ban state.
func (s *Service) UnbanUser(actorID, targetID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return err
	}
	user, ok := data.Users[targetID]
	if !ok {
		return fmt.Errorf("canonical user %q not found", targetID)
	}
	if !user.IsBanned && user.BannedAt.IsZero() && user.BannedBy == "" && user.BanReason == "" {
		return nil
	}
	user.IsBanned = false
	user.BannedAt = time.Time{}
	user.BannedBy = ""
	user.BanReason = ""
	user.UpdatedAt = time.Now().UTC()
	data.Users[targetID] = user
	return s.saveLocked(data)
}

// SpeakerLine returns a deterministic speaker line for the canonical user.
func (s *Service) SpeakerLine(canonicalUserID string) (string, error) {
	accounts, err := s.AccountsForUser(canonicalUserID)
	if err != nil {
		return "", err
	}
	return FormatSpeakerLine(accounts), nil
}

// LinkAccount links a new external account to a canonical user, merging users when needed.
func (s *Service) LinkAccount(canonicalUserID, gateway, identifier, displayName string) (LinkResult, error) {
	identifier, err := NormalizeIdentifier(gateway, identifier)
	if err != nil {
		return LinkResult{}, err
	}

	gateway = strings.ToLower(strings.TrimSpace(gateway))
	key := accountKey(gateway, identifier)

	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return LinkResult{}, err
	}

	current, ok := data.Users[canonicalUserID]
	if !ok {
		return LinkResult{}, fmt.Errorf("canonical user %q not found", canonicalUserID)
	}

	for _, account := range current.Accounts {
		if account.Gateway == gateway {
			if account.Identifier == identifier {
				return LinkResult{
					CanonicalUserID: canonicalUserID,
					AlreadyLinked:   true,
					LinkedAccount:   account,
				}, nil
			}
			return LinkResult{}, fmt.Errorf("%s is already linked on this user as %s", gateway, account.Identifier)
		}
	}

	if existingOwner, ok := data.AccountIndex[key]; ok {
		if existingOwner == canonicalUserID {
			for _, account := range current.Accounts {
				if account.Gateway == gateway && account.Identifier == identifier {
					return LinkResult{CanonicalUserID: canonicalUserID, AlreadyLinked: true, LinkedAccount: account}, nil
				}
			}
		}

		other := data.Users[existingOwner]
		for _, account := range other.Accounts {
			if account.Gateway == gateway && account.Identifier == identifier {
				break
			}
		}
		for _, account := range current.Accounts {
			if account.Gateway == gateway {
				return LinkResult{}, fmt.Errorf("cannot merge because this user already has a %s account linked", gateway)
			}
		}

		currentGateways := make(map[string]string, len(current.Accounts))
		for _, account := range current.Accounts {
			currentGateways[account.Gateway] = account.Identifier
		}
		for _, account := range other.Accounts {
			if identifier, ok := currentGateways[account.Gateway]; ok && identifier != account.Identifier {
				return LinkResult{}, fmt.Errorf("cannot merge because both users already have %s accounts linked", account.Gateway)
			}
		}

		if err := s.memories.MergeUsers(canonicalUserID, existingOwner); err != nil {
			return LinkResult{}, err
		}
		s.log.Info("account_link.users.merged", "merged linked users", config.F("source_user_id", existingOwner), config.F("target_user_id", canonicalUserID), config.F("account", key))

		mergedUser := current
		mergedUser.IsAdmin = current.IsAdmin || other.IsAdmin
		if !current.IsBanned && other.IsBanned {
			mergedUser.IsBanned = true
			mergedUser.BannedAt = other.BannedAt
			mergedUser.BannedBy = other.BannedBy
			mergedUser.BanReason = other.BanReason
		} else if current.IsBanned {
			mergedUser.IsBanned = true
		}
		seen := make(map[string]struct{}, len(mergedUser.Accounts))
		for _, account := range mergedUser.Accounts {
			seen[accountKey(account.Gateway, account.Identifier)] = struct{}{}
		}
		for _, account := range other.Accounts {
			account.DisplayName = chooseDisplayName(account.DisplayName, displayName)
			acctKey := accountKey(account.Gateway, account.Identifier)
			if _, ok := seen[acctKey]; ok {
				continue
			}
			mergedUser.Accounts = append(mergedUser.Accounts, account)
			seen[acctKey] = struct{}{}
		}
		mergedUser.UpdatedAt = time.Now().UTC()
		data.Users[canonicalUserID] = mergedUser
		delete(data.Users, existingOwner)
		for acctKey, owner := range data.AccountIndex {
			if owner == existingOwner {
				data.AccountIndex[acctKey] = canonicalUserID
			}
		}

		if err := s.saveLocked(data); err != nil {
			return LinkResult{}, err
		}
		if err := s.memories.SyncSpeakerIntro(canonicalUserID, FormatSpeakerLine(mergedUser.Accounts)); err != nil {
			return LinkResult{}, err
		}

		linkedAccount, _ := findAccount(mergedUser.Accounts, gateway, identifier)
		return LinkResult{
			CanonicalUserID: canonicalUserID,
			Merged:          true,
			LinkedAccount:   linkedAccount,
		}, nil
	}

	linked := LinkedAccount{
		Gateway:     gateway,
		Identifier:  identifier,
		DisplayName: displayName,
		LinkedAt:    time.Now().UTC(),
		Verified:    false,
	}
	current.Accounts = append(current.Accounts, linked)
	current.UpdatedAt = time.Now().UTC()
	data.Users[canonicalUserID] = current
	data.AccountIndex[key] = canonicalUserID

	if err := s.saveLocked(data); err != nil {
		return LinkResult{}, err
	}
	if err := s.memories.SyncSpeakerIntro(canonicalUserID, FormatSpeakerLine(current.Accounts)); err != nil {
		return LinkResult{}, err
	}
	s.log.Info("account_link.account.linked", "linked account", config.F("account", key), config.F("target_user_id", canonicalUserID))

	return LinkResult{CanonicalUserID: canonicalUserID, LinkedAccount: linked}, nil
}

// DisconnectAccount removes a linked external account from a canonical user.
func (s *Service) DisconnectAccount(canonicalUserID, gateway, identifier string) error {
	identifier, err := NormalizeIdentifier(gateway, identifier)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return err
	}

	user, ok := data.Users[canonicalUserID]
	if !ok {
		return fmt.Errorf("canonical user %q not found", canonicalUserID)
	}
	if len(user.Accounts) <= 1 {
		return fmt.Errorf("cannot disconnect the last linked account")
	}

	key := accountKey(gateway, identifier)
	kept := user.Accounts[:0]
	removed := false
	for _, account := range user.Accounts {
		if account.Gateway == strings.ToLower(gateway) && account.Identifier == identifier {
			removed = true
			continue
		}
		kept = append(kept, account)
	}
	if !removed {
		return fmt.Errorf("linked account not found")
	}
	user.Accounts = kept
	user.UpdatedAt = time.Now().UTC()
	data.Users[canonicalUserID] = user
	delete(data.AccountIndex, key)

	if err := s.saveLocked(data); err != nil {
		return err
	}
	s.log.Info("account_link.account.disconnected", "disconnected account", config.F("account", key), config.F("target_user_id", canonicalUserID))
	return s.memories.SyncSpeakerIntro(canonicalUserID, FormatSpeakerLine(user.Accounts))
}

func (s *Service) loadLocked() (fileData, error) {
	if err := s.Initialize(); err != nil {
		return fileData{}, err
	}
	return s.db.LoadAccountLinks()
}

func (s *Service) saveLocked(data fileData) error {
	if err := s.Initialize(); err != nil {
		return err
	}
	return s.db.ReplaceAccountLinks(data)
}

func (s *Service) initialize() error {
	db, err := database.Open(s.path, s.log)
	if err != nil {
		return err
	}
	if err := db.MigrateLegacyAccountLinks(s.legacyPath); err != nil {
		db.Close() // nolint:errcheck
		return err
	}
	s.db = db
	return nil
}

func newCanonicalUserID() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate canonical user ID: %w", err)
	}
	return "usr_" + hex.EncodeToString(b), nil
}

func findAccount(accounts []LinkedAccount, gateway, identifier string) (LinkedAccount, bool) {
	for _, account := range accounts {
		if account.Gateway == gateway && account.Identifier == identifier {
			return account, true
		}
	}
	return LinkedAccount{}, false
}

func summarizeUser(canonicalID string, user UserRecord) UserSummary {
	accounts := append([]LinkedAccount(nil), user.Accounts...)
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Gateway == accounts[j].Gateway {
			return accounts[i].Identifier < accounts[j].Identifier
		}
		return accounts[i].Gateway < accounts[j].Gateway
	})

	return UserSummary{
		CanonicalUserID: canonicalID,
		Intro:           FormatSpeakerLine(accounts),
		Accounts:        accounts,
		CreatedAt:       user.CreatedAt,
		UpdatedAt:       user.UpdatedAt,
		IsAdmin:         user.IsAdmin,
		IsBanned:        user.IsBanned,
		BannedBy:        user.BannedBy,
		BanReason:       user.BanReason,
	}
}

func chooseDisplayName(existing, requested string) string {
	if existing != "" {
		return existing
	}
	return requested
}

// FormatSpeakerLine formats a stable speaker line from linked gateway accounts.
func FormatSpeakerLine(accounts []LinkedAccount) string {
	var imessageName string
	var discordName string
	var websocketName string

	for _, account := range accounts {
		name := strings.TrimSpace(account.DisplayName)
		if name == "" {
			continue
		}

		switch account.Gateway {
		case "imessage":
			if imessageName == "" {
				imessageName = name
			}
		case "discord":
			if discordName == "" {
				discordName = name
			}
		case "websocket":
			if websocketName == "" {
				websocketName = name
			}
		}
	}

	switch {
	case imessageName != "" && discordName != "":
		if strings.EqualFold(imessageName, discordName) {
			return fmt.Sprintf("You are speaking with %s.", imessageName)
		}
		return fmt.Sprintf("You are speaking with %s aka %s.", imessageName, discordName)
	case imessageName != "":
		return fmt.Sprintf("You are speaking with %s.", imessageName)
	case discordName != "":
		return fmt.Sprintf("You are speaking with %s.", discordName)
	case websocketName != "":
		return fmt.Sprintf("You are speaking with %s.", websocketName)
	default:
		return "You are speaking with a returning user."
	}
}
