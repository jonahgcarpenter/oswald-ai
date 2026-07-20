// Package privacy implements deterministic authenticated self-service privacy operations.
package privacy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/privacyruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

const challengeTTL = 10 * time.Minute

var codeEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// Request contains transport facts that cannot be inferred from the principal.
type Request struct {
	RequestID  string
	Principal  identity.Principal
	IsDirect   bool
	SessionKey string
}

// Challenge is a confirmation required for a destructive bulk operation.
type Challenge struct {
	Code      string
	ExpiresAt time.Time
}

// ExportPart is one ordered, byte-exact part of a user export.
type ExportPart struct {
	Filename string
	MIMEType string
	Data     []byte
}

// Export is a complete bounded user export. Filename and Data are populated
// for single-file exports for compatibility; Parts is always canonical.
type Export struct {
	Filename string
	Data     []byte
	Parts    []ExportPart
}

// NewExport builds deterministic deliverable parts from complete export JSON.
func NewExport(data []byte, now time.Time) (Export, error) {
	if len(data) == 0 {
		return Export{}, fmt.Errorf("privacy export data is empty")
	}
	if len(data) > commands.MaxTotalAttachmentBytes {
		return Export{}, fmt.Errorf("privacy export exceeds the %d-byte total attachment limit", commands.MaxTotalAttachmentBytes)
	}
	base := "oswald-user-export-" + now.UTC().Format("20060102T150405Z") + ".json"
	if len(data) <= commands.MaxAttachmentBytes {
		part := ExportPart{Filename: base, MIMEType: "application/json", Data: data}
		return Export{Filename: base, Data: data, Parts: []ExportPart{part}}, nil
	}
	partCount := (len(data) + commands.MaxAttachmentBytes - 1) / commands.MaxAttachmentBytes
	parts := make([]ExportPart, 0, partCount)
	for start, number := 0, 1; start < len(data); start, number = start+commands.MaxAttachmentBytes, number+1 {
		end := start + commands.MaxAttachmentBytes
		if end > len(data) {
			end = len(data)
		}
		parts = append(parts, ExportPart{
			Filename: fmt.Sprintf("%s.part%03d", base, number),
			MIMEType: "application/octet-stream",
			Data:     data[start:end],
		})
	}
	return Export{Parts: parts}, nil
}

// Service coordinates stable identity resolution with canonical privacy storage.
type Service struct {
	accounts *accountlinking.Service
	memory   *usermemory.Store
	policy   config.RetentionPolicy
	log      *config.Logger
	now      func() time.Time
	random   io.Reader
}

// NewService creates a deterministic privacy service.
func NewService(accounts *accountlinking.Service, memory *usermemory.Store, policy config.RetentionPolicy, log *config.Logger) (*Service, error) {
	if accounts == nil || memory == nil {
		return nil, fmt.Errorf("privacy account and memory stores are required")
	}
	return &Service{accounts: accounts, memory: memory, policy: policy, log: log, now: time.Now, random: rand.Reader}, nil
}

// Inspect returns one bounded metadata-only page.
func (s *Service) Inspect(ctx context.Context, req Request, section string, page int) (usermemory.PrivacyPage, error) {
	started := time.Now()
	userID, _, err := s.authorize(req)
	if err != nil {
		s.observe("inspect", started, 0, err)
		return usermemory.PrivacyPage{}, err
	}
	result, err := s.memory.InspectPrivacy(ctx, userID, section, page)
	s.observe("inspect", started, len(result.Items), err)
	return result, err
}

// Export creates a stable JSON export from one database snapshot.
func (s *Service) Export(ctx context.Context, req Request) (Export, error) {
	started := time.Now()
	userID, actorHash, err := s.authorize(req)
	if err != nil {
		s.observe("export_user", started, 0, err)
		return Export{}, err
	}
	now := s.now().UTC()
	if err := s.memory.PrivacyExportPreflight(ctx, userID, commands.MaxTotalAttachmentBytes); err != nil {
		s.observe("export_user", started, 0, err)
		return Export{}, err
	}
	data, err := s.memory.ExportPrivacy(ctx, userID, now)
	if err != nil {
		s.observe("export_user", started, 0, err)
		return Export{}, err
	}
	export, err := NewExport(data, now)
	if err != nil {
		s.observe("export_user", started, 0, err)
		return Export{}, err
	}
	if err := s.memory.RecordPrivacyExport(ctx, userID, actorHash, req.RequestID, now); err != nil {
		s.observe("export_user", started, 0, err)
		return Export{}, err
	}
	s.observe("export_user", started, len(export.Parts), nil)
	return export, nil
}

// ForgetMemory applies the configured grace policy to an exact memory ID.
func (s *Service) ForgetMemory(ctx context.Context, req Request, id int64) (string, error) {
	started := time.Now()
	userID, actorHash, err := s.authorize(req)
	if err != nil {
		s.observe("forget_memory", started, 0, err)
		return "", err
	}
	state, err := s.memory.ForgetMemory(ctx, userID, actorHash, id, req.RequestID, s.now().UTC(), s.policy)
	s.observe("forget_memory", started, 1, err)
	return state, err
}

// DeleteMemory irreversibly deletes an exact memory ID.
func (s *Service) DeleteMemory(ctx context.Context, req Request, id int64) (string, error) {
	started := time.Now()
	userID, actorHash, err := s.authorize(req)
	if err != nil {
		s.observe("delete_memory", started, 0, err)
		return "", err
	}
	state, err := s.memory.DeleteMemory(ctx, userID, actorHash, id, req.RequestID, s.now().UTC())
	s.observe("delete_memory", started, 1, err)
	return state, err
}

// DeleteCandidate irreversibly deletes an exact candidate ID.
func (s *Service) DeleteCandidate(ctx context.Context, req Request, id int64) error {
	started := time.Now()
	userID, actorHash, err := s.authorize(req)
	if err != nil {
		s.observe("delete_candidate", started, 0, err)
		return err
	}
	err = s.memory.DeleteCandidate(ctx, userID, actorHash, id, req.RequestID, s.now().UTC())
	s.observe("delete_candidate", started, 1, err)
	return err
}

// DeleteSession deletes the current session's current generation only.
func (s *Service) DeleteSession(ctx context.Context, req Request) (int, error) {
	started := time.Now()
	userID, actorHash, err := s.authorize(req)
	if err != nil {
		s.observe("delete_session", started, 0, err)
		return 0, err
	}
	generation, err := s.memory.DeleteSessionPrivacy(ctx, userID, actorHash, req.SessionKey, req.RequestID, s.now().UTC())
	s.observe("delete_session", started, 1, err)
	return generation, err
}

// Invalidation resolves the runtime state associated with a privacy mutation.
// A nil sessionIDs slice selects all sessions; a non-nil slice is used exactly.
func (s *Service) Invalidation(ctx context.Context, req Request, sessionIDs []string) (privacyruntime.Event, error) {
	userID, _, err := s.authorize(req)
	if err != nil {
		return privacyruntime.Event{}, err
	}
	accounts, err := s.accounts.AccountsForUser(userID)
	if err != nil {
		return privacyruntime.Event{}, err
	}
	externalIdentities := make([]string, 0, len(accounts))
	for _, account := range accounts {
		externalIdentities = append(externalIdentities, account.Gateway+":"+account.Identifier)
	}
	if sessionIDs == nil {
		sessionIDs, err = s.memory.PrivacySessionIDs(ctx, userID)
		if err != nil {
			return privacyruntime.Event{}, err
		}
	}
	return privacyruntime.Event{ExternalIdentities: externalIdentities, SessionIDs: append([]string(nil), sessionIDs...)}, nil
}

// BeginDeleteAllMemories creates a one-time confirmation challenge.
func (s *Service) BeginDeleteAllMemories(ctx context.Context, req Request) (Challenge, error) {
	return s.begin(ctx, req, "delete_all_memories")
}

// BeginDeleteAccount creates a one-time confirmation challenge.
func (s *Service) BeginDeleteAccount(ctx context.Context, req Request) (Challenge, error) {
	return s.begin(ctx, req, "delete_user")
}

// Confirm consumes and executes an identity-bound confirmation challenge.
func (s *Service) Confirm(ctx context.Context, req Request, code string) (usermemory.PrivacyConfirmation, error) {
	started := time.Now()
	userID, actorHash, err := s.authorize(req)
	if err != nil {
		s.observe("confirm", started, 0, err)
		return usermemory.PrivacyConfirmation{}, err
	}
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		err := fmt.Errorf("confirmation code is required")
		s.observe("confirm", started, 0, err)
		return usermemory.PrivacyConfirmation{}, err
	}
	// Capture invalidation scope before a delete-account transaction removes it.
	invalidation, err := s.Invalidation(ctx, req, nil)
	if err != nil {
		s.observe("confirm", started, 0, err)
		return usermemory.PrivacyConfirmation{}, err
	}
	var result usermemory.PrivacyConfirmation
	err = s.accounts.RunAuthenticatedUserMutation(req.Principal, func(lockedUserID string) error {
		if lockedUserID != userID {
			return accountlinking.ErrPrincipalMismatch
		}
		var confirmErr error
		result, confirmErr = s.memory.ConfirmPrivacyChallenge(ctx, lockedUserID, actorHash, hash(code), req.RequestID, s.now().UTC())
		return confirmErr
	})
	if err != nil {
		s.observe("confirm", started, 0, err)
		return usermemory.PrivacyConfirmation{}, err
	}
	if result.DeletedUserID != "" {
		s.accounts.UserErasureCommitted(result.DeletedUserID)
	}
	if len(result.ExternalIdentities) == 0 {
		result.ExternalIdentities = invalidation.ExternalIdentities
	}
	if len(result.SessionIDs) == 0 {
		result.SessionIDs = invalidation.SessionIDs
	}
	s.observe(result.OperationType, started, 1, nil)
	return result, nil
}

func (s *Service) begin(ctx context.Context, req Request, operation string) (Challenge, error) {
	started := time.Now()
	userID, actorHash, err := s.authorize(req)
	if err != nil {
		s.observe(operation, started, 0, err)
		return Challenge{}, err
	}
	codeBytes := make([]byte, 8)
	operationBytes := make([]byte, 16)
	if _, err := io.ReadFull(s.random, codeBytes); err != nil {
		s.observe(operation, started, 0, err)
		return Challenge{}, fmt.Errorf("generate privacy challenge: %w", err)
	}
	if _, err := io.ReadFull(s.random, operationBytes); err != nil {
		s.observe(operation, started, 0, err)
		return Challenge{}, fmt.Errorf("generate privacy operation id: %w", err)
	}
	code := codeEncoding.EncodeToString(codeBytes)
	now := s.now().UTC()
	expires := now.Add(challengeTTL)
	operationID := "prv_" + hex.EncodeToString(operationBytes)
	targetDigest := hash(operation + "\x00" + userID)
	if _, err := s.memory.CreatePrivacyChallenge(ctx, userID, actorHash, operationID, operation, targetDigest, hash(code), now, expires); err != nil {
		s.observe(operation, started, 0, err)
		return Challenge{}, err
	}
	s.observe(operation, started, 1, nil)
	return Challenge{Code: code, ExpiresAt: expires}, nil
}

func (s *Service) authorize(req Request) (string, string, error) {
	if !req.IsDirect {
		return "", "", fmt.Errorf("privacy commands require a direct conversation")
	}
	if !req.Principal.Valid() || !req.Principal.Authenticated() {
		return "", "", fmt.Errorf("privacy commands require an authenticated identity")
	}
	identifier, err := accountlinking.NormalizeIdentifier(req.Principal.Gateway, req.Principal.ExternalID)
	if err != nil {
		return "", "", err
	}
	userID, err := s.accounts.ResolvePrincipal(req.Principal)
	if err != nil {
		return "", "", err
	}
	if userID != req.Principal.CanonicalUserID {
		return "", "", accountlinking.ErrPrincipalMismatch
	}
	return userID, hash(strings.ToLower(req.Principal.Gateway) + "\x00" + identifier), nil
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (s *Service) observe(operation string, started time.Time, count int, err error) {
	if s.log == nil {
		return
	}
	status := "ok"
	if err != nil {
		status = "error"
	}
	fields := []config.Field{config.F("operation_type", operation), config.F("record_count", count), config.F("duration_ms", time.Since(started).Milliseconds()), config.F("status", status)}
	if err != nil {
		s.log.Warn("privacy.operation.complete", "privacy operation completed", fields...)
		return
	}
	s.log.Info("privacy.operation.complete", "privacy operation completed", fields...)
}
