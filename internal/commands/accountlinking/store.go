package accountlinking

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

// Service manages canonical user IDs and linked gateway accounts.
type Service struct {
	path       string
	legacyPath string
	memories   *usermemory.Store
	log        *config.Logger
	db         *database.DB
	mcp        MCPUserMerger
	now        func() time.Time
	random     io.Reader
	mu         sync.Mutex
	initOnce   sync.Once
	initErr    error
}

// NewService creates a new account-link service backed by a SQLite database on disk.
func NewService(path string, memories *usermemory.Store, mcp MCPUserMerger, log *config.Logger) *Service {
	legacyPath := filepath.Join(filepath.Dir(path), "links.json")
	if path == config.DefaultAccountLinkPath {
		legacyPath = config.DefaultLegacyAccountLinkPath
	}
	return &Service{path: path, legacyPath: legacyPath, memories: memories, mcp: mcp, log: log, now: time.Now, random: rand.Reader}
}

// Initialize prepares the account-link database and migrates the legacy JSON store when present.
func (s *Service) Initialize() error {
	s.initOnce.Do(func() {
		s.initErr = s.initialize()
	})
	return s.initErr
}

// Close releases the account-link service's database handle.
func (s *Service) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
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

	s.log.Info("account_link.canonical_user.created", "created canonical user", config.F("target_user_id", canonicalID), config.F("account", key))
	return canonicalID, nil
}

// ResolveAccount returns the current canonical owner without creating an account.
func (s *Service) ResolveAccount(gateway, identifier string) (string, bool, error) {
	identifier, err := NormalizeIdentifier(gateway, identifier)
	if err != nil {
		return "", false, err
	}
	gateway = strings.ToLower(strings.TrimSpace(gateway))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureInitializedLocked(); err != nil {
		return "", false, err
	}
	var owner string
	err = s.db.SQL().QueryRow(`SELECT canonical_user_id FROM linked_accounts WHERE gateway = ? AND identifier = ?`, gateway, identifier).Scan(&owner)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve linked account: %w", err)
	}
	return owner, true, nil
}

// ResolvePrincipal re-resolves an authenticated external identity to its current
// canonical owner. The canonical ID carried by principal is intentionally ignored.
func (s *Service) ResolvePrincipal(principal identity.Principal) (string, error) {
	if !principal.Valid() || !principal.Authenticated() {
		return "", fmt.Errorf("privacy operation requires an authenticated identity")
	}
	owner, found, err := s.ResolveAccount(principal.Gateway, principal.ExternalID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrPrincipalMismatch
	}
	return owner, nil
}

// RunAuthenticatedUserMutation serializes a privacy mutation with account
// creation, linking, merging, and display-name updates while re-resolving the
// external identity under the same account graph lock.
func (s *Service) RunAuthenticatedUserMutation(principal identity.Principal, fn func(string) error) error {
	if !principal.Valid() || !principal.Authenticated() {
		return fmt.Errorf("privacy operation requires an authenticated identity")
	}
	identifier, err := NormalizeIdentifier(principal.Gateway, principal.ExternalID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked()
	if err != nil {
		return err
	}
	owner, ok := data.AccountIndex[accountKey(principal.Gateway, identifier)]
	if !ok {
		return ErrPrincipalMismatch
	}
	return fn(owner)
}

// UserErasureCommitted invalidates runtime state after a committed self-erasure.
func (s *Service) UserErasureCommitted(canonicalUserID string) {
	if s.mcp != nil {
		s.mcp.UserDeleteCommitted(canonicalUserID)
	}
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
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked()
	if err != nil {
		return err
	}
	return s.setAdminLocked(data, actorID, targetID, isAdmin)
}

// SetAdminAs updates admin state after atomically re-resolving the authenticated actor.
func (s *Service) SetAdminAs(principal identity.Principal, targetID string, isAdmin bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked()
	if err != nil {
		return err
	}
	actorID, err := authenticatedAdminActor(data, principal)
	if err != nil {
		return err
	}
	return s.setAdminLocked(data, actorID, targetID, isAdmin)
}

func (s *Service) setAdminLocked(data fileData, actorID, targetID string, isAdmin bool) error {
	if actorID == targetID && !isAdmin {
		return fmt.Errorf("cannot remove admin from yourself")
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
	if err := s.saveLocked(data); err != nil {
		return err
	}
	if isAdmin {
		s.log.Info("account_link.user.admin_granted", "granted user admin access", config.F("actor_user_id", actorID), config.F("target_user_id", targetID), config.F("status", "ok"))
	} else {
		s.log.Info("account_link.user.admin_revoked", "revoked user admin access", config.F("actor_user_id", actorID), config.F("target_user_id", targetID), config.F("status", "ok"))
	}
	return nil
}

// BanUser marks a canonical user as banned.
func (s *Service) BanUser(actorID, targetID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked()
	if err != nil {
		return err
	}
	return s.banUserLocked(data, actorID, targetID, reason)
}

// BanUserAs bans a user after atomically re-resolving the authenticated actor.
func (s *Service) BanUserAs(principal identity.Principal, targetID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked()
	if err != nil {
		return err
	}
	actorID, err := authenticatedAdminActor(data, principal)
	if err != nil {
		return err
	}
	return s.banUserLocked(data, actorID, targetID, reason)
}

func (s *Service) banUserLocked(data fileData, actorID, targetID, reason string) error {
	if actorID == targetID {
		return fmt.Errorf("cannot ban yourself")
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
	if err := s.saveLocked(data); err != nil {
		return err
	}
	s.log.Info("account_link.user.banned", "banned user", config.F("actor_user_id", actorID), config.F("target_user_id", targetID), config.F("status", "ok"))
	return nil
}

// UnbanUser clears a canonical user's ban state.
func (s *Service) UnbanUser(actorID, targetID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked()
	if err != nil {
		return err
	}
	return s.unbanUserLocked(data, actorID, targetID)
}

// UnbanUserAs unbans a user after atomically re-resolving the authenticated actor.
func (s *Service) UnbanUserAs(principal identity.Principal, targetID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked()
	if err != nil {
		return err
	}
	actorID, err := authenticatedAdminActor(data, principal)
	if err != nil {
		return err
	}
	return s.unbanUserLocked(data, actorID, targetID)
}

func (s *Service) unbanUserLocked(data fileData, actorID, targetID string) error {
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
	if err := s.saveLocked(data); err != nil {
		return err
	}
	s.log.Info("account_link.user.unbanned", "unbanned user", config.F("actor_user_id", actorID), config.F("target_user_id", targetID), config.F("status", "ok"))
	return nil
}

// DeleteUser removes a canonical user and all data owned by that user.
func (s *Service) DeleteUser(actorID, targetID string) error {
	targetID = strings.TrimSpace(targetID)
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked()
	if err != nil {
		return err
	}
	return s.deleteUserLocked(data, actorID, targetID)
}

// DeleteUserAs deletes a user after atomically re-resolving the authenticated actor.
func (s *Service) DeleteUserAs(principal identity.Principal, targetID string) error {
	_, err := s.DeleteUserAsWithInvalidation(principal, targetID)
	return err
}

// DeleteUserAsWithInvalidation deletes a user and returns its pre-erasure runtime scope.
func (s *Service) DeleteUserAsWithInvalidation(principal identity.Principal, targetID string) (ErasureDescriptor, error) {
	return s.DeleteUserAsWithDurableInvalidation(principal, targetID, "admin-delete:"+targetID+":"+time.Now().UTC().Format(time.RFC3339Nano))
}

// DeleteUserAsWithDurableInvalidation deletes a user and durably queues its runtime scope.
func (s *Service) DeleteUserAsWithDurableInvalidation(principal identity.Principal, targetID, operationID string) (ErasureDescriptor, error) {
	if strings.TrimSpace(operationID) == "" {
		operationID = "admin-delete:" + strings.TrimSpace(targetID) + ":" + time.Now().UTC().Format(time.RFC3339Nano)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked()
	if err != nil {
		return ErasureDescriptor{}, err
	}
	actorID, err := authenticatedAdminActor(data, principal)
	if err != nil {
		return ErasureDescriptor{}, err
	}
	return s.deleteUserLockedWithInvalidation(data, actorID, strings.TrimSpace(targetID), operationID)
}

func (s *Service) deleteUserLocked(data fileData, actorID, targetID string) error {
	_, err := s.deleteUserLockedWithInvalidation(data, actorID, targetID, "admin-delete:"+targetID+":"+time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Service) deleteUserLockedWithInvalidation(data fileData, actorID, targetID, operationID string) (ErasureDescriptor, error) {
	if targetID == "" {
		return ErasureDescriptor{}, fmt.Errorf("canonical user ID cannot be empty")
	}
	if actorID == targetID {
		return ErasureDescriptor{}, fmt.Errorf("cannot delete yourself")
	}
	user, ok := data.Users[targetID]
	if !ok {
		return ErasureDescriptor{}, fmt.Errorf("canonical user %q not found", targetID)
	}

	ctx := context.Background()
	var invalidation usermemory.UserErasureInvalidation
	if err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if s.mcp != nil {
			if err := s.mcp.DeleteUserTx(ctx, tx, targetID); err != nil {
				return err
			}
		}
		var err error
		invalidation, err = s.memories.EraseUserWithInvalidationTx(ctx, tx, targetID, operationID, time.Now().UTC())
		return err
	}); err != nil {
		return ErasureDescriptor{}, err
	}
	if s.mcp != nil {
		s.mcp.UserDeleteCommitted(targetID)
	}

	s.log.Info("account_link.user.deleted", "deleted user", config.F("actor_user_id", actorID), config.F("target_user_id", targetID), config.F("account_count", len(user.Accounts)), config.F("status", "ok"))
	return ErasureDescriptor{ExternalIdentities: invalidation.ExternalIdentities, SessionIDs: invalidation.SessionIDs}, nil
}

func authenticatedAdminActor(data fileData, principal identity.Principal) (string, error) {
	if !principal.Valid() || !principal.Authenticated() {
		return "", fmt.Errorf("admin command requires an authenticated identity")
	}
	identifier, err := NormalizeIdentifier(principal.Gateway, principal.ExternalID)
	if err != nil {
		return "", err
	}
	actorID, ok := data.AccountIndex[accountKey(principal.Gateway, identifier)]
	if !ok {
		return "", ErrPrincipalMismatch
	}
	actor, ok := data.Users[actorID]
	if !ok {
		return "", ErrPrincipalMismatch
	}
	if !actor.IsAdmin {
		return "", fmt.Errorf("canonical user %q is not an admin", actorID)
	}
	return actorID, nil
}

// SpeakerLine returns a deterministic speaker line for the canonical user.
func (s *Service) SpeakerLine(canonicalUserID string) (string, error) {
	accounts, err := s.AccountsForUser(canonicalUserID)
	if err != nil {
		return "", err
	}
	return FormatSpeakerLine(accounts), nil
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
