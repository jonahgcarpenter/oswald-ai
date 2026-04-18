package accountlink

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/usermemory"
)

// Service manages canonical user IDs and linked gateway accounts.
type Service struct {
	path     string
	memories *usermemory.Store
	log      *config.Logger
	mu       sync.Mutex
}

// NewService creates a new account-link service backed by a JSON file on disk.
func NewService(path string, memories *usermemory.Store, log *config.Logger) *Service {
	return &Service{path: path, memories: memories, log: log}
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

	s.log.Debug("AccountLink: created canonical user %s for %s", canonicalID, key)
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

		mergedUser := current
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
	return s.memories.SyncSpeakerIntro(canonicalUserID, FormatSpeakerLine(user.Accounts))
}

func (s *Service) loadLocked() (fileData, error) {
	data := fileData{
		Version:      1,
		Users:        make(map[string]UserRecord),
		AccountIndex: make(map[string]string),
	}

	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return data, nil
	}
	if err != nil {
		return fileData{}, fmt.Errorf("failed to read account link store: %w", err)
	}
	if len(raw) == 0 {
		return data, nil
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		sanitized := sanitizeJSON(raw)
		if jsonErr := json.Unmarshal(sanitized, &data); jsonErr != nil {
			backupPath := s.path + ".corrupt-" + time.Now().UTC().Format("20060102T150405Z")
			if writeErr := os.WriteFile(backupPath, raw, 0o644); writeErr != nil {
				s.log.Warn("AccountLink: failed to back up corrupt store %s: %v", backupPath, writeErr)
			}
			return fileData{}, fmt.Errorf("failed to decode account link store: %w", err)
		}
		if err := os.WriteFile(s.path, sanitized, 0o644); err != nil {
			s.log.Warn("AccountLink: recovered malformed JSON in memory but could not rewrite store: %v", err)
		} else {
			s.log.Warn("AccountLink: repaired malformed JSON in %s", s.path)
		}
	}
	if data.Users == nil {
		data.Users = make(map[string]UserRecord)
	}
	if data.AccountIndex == nil {
		data.AccountIndex = make(map[string]string)
	}
	if data.Version == 0 {
		data.Version = 1
	}
	return data, nil
}

var trailingCommaRE = regexp.MustCompile(`,(\s*[}\]])`)

func sanitizeJSON(raw []byte) []byte {
	trimmed := bytesTrimSpace(raw)
	if len(trimmed) == 0 {
		return raw
	}
	return trailingCommaRE.ReplaceAll(trimmed, []byte("$1"))
}

func bytesTrimSpace(raw []byte) []byte {
	return []byte(strings.TrimSpace(string(raw)))
}

func (s *Service) saveLocked(data fileData) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("failed to create account link directory: %w", err)
	}

	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode account link store: %w", err)
	}
	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, raw, 0o644); err != nil {
		return fmt.Errorf("failed to write account link store: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("failed to replace account link store: %w", err)
	}
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
