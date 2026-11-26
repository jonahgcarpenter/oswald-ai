package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	sessionModelPrefix     = "oswald:v1:session_model:"
	sessionModelValueLimit = 1024
	sessionModelFieldRunes = 256
)

// errSessionModelValue classifies a present-but-unreadable conversation model
// pin. Its text names the state_meta schema path and never the stored bytes.
var errSessionModelValue = errors.New("state_meta session_model value is invalid")

// unreadableSessionModel marks a stored pin value that exists but cannot be
// interpreted. UnreadablePin is the duck-typed marker consumed by the agent
// model router's error classification, so this package needs no dependency on
// internal/agent.
type unreadableSessionModel struct{ field string }

func (e *unreadableSessionModel) Error() string {
	return errSessionModelValue.Error() + ": " + e.field
}

func (e *unreadableSessionModel) Unwrap() error { return errSessionModelValue }

func (e *unreadableSessionModel) UnreadablePin() bool { return true }

// profileSessionModel is the version-1 conversation model pin stored in
// state_meta. Provider is the bare provider name, without a custom: prefix.
type profileSessionModel struct {
	Version  int    `json:"version"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// sessionModelKey addresses a conversation's model pin by source and session
// key remainder. It never includes sessions.id, so pins survive /new and
// ordinary TTL sweeps of the session row.
func sessionModelKey(source, remainder string) string {
	return sessionModelPrefix + source + ":" + remainder
}

func validSessionModelField(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && utf8.RuneCountInString(value) <= sessionModelFieldRunes
}

func decodeSessionModel(encoded string) (profileSessionModel, error) {
	var pin profileSessionModel
	if len(encoded) > sessionModelValueLimit {
		return pin, &unreadableSessionModel{field: "size"}
	}
	var required struct {
		Version  *int    `json:"version"`
		Provider *string `json:"provider"`
		Model    *string `json:"model"`
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	// Decoder errors are reported by classification only: their text can quote
	// stored bytes, which must never leave the database in an error.
	if err := decoder.Decode(&required); err != nil {
		return pin, &unreadableSessionModel{field: "json"}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return pin, &unreadableSessionModel{field: "json"}
	}
	if required.Version == nil || *required.Version != 1 {
		return pin, &unreadableSessionModel{field: "version"}
	}
	if required.Provider == nil || !validSessionModelField(*required.Provider) {
		return pin, &unreadableSessionModel{field: "provider"}
	}
	if required.Model == nil || !validSessionModelField(*required.Model) {
		return pin, &unreadableSessionModel{field: "model"}
	}
	pin = profileSessionModel{Version: *required.Version, Provider: *required.Provider, Model: *required.Model}
	return pin, nil
}

// SessionModel returns the persisted conversation model pin. found is false
// only when no pin exists. A present-but-unreadable value fails closed as an
// error naming only the schema path, never the stored value.
func (s *ProfileStore) SessionModel(ctx context.Context, owner, sessionKey string) (provider, model string, found bool, err error) {
	source, err := s.scope(owner, sessionKey)
	if err != nil {
		return "", "", false, err
	}
	_, remainder, _ := strings.Cut(sessionKey, ":")
	var encoded sql.NullString
	err = s.db.SQL().QueryRowContext(ctx, `SELECT value FROM state_meta WHERE key=?`, sessionModelKey(source, remainder)).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("read profile session model pin: %w", err)
	}
	if !encoded.Valid {
		return "", "", false, &unreadableSessionModel{field: "null"}
	}
	pin, err := decodeSessionModel(encoded.String)
	if err != nil {
		return "", "", false, err
	}
	return pin.Provider, pin.Model, true, nil
}

// SetSessionModel persists the conversation's model pin. Values are trimmed,
// required non-empty, and bounded; the encoded row is the runtime-owned
// oswald:v1:session_model key family and survives /new and restarts.
func (s *ProfileStore) SetSessionModel(ctx context.Context, owner, sessionKey, provider, model string) error {
	source, err := s.scope(owner, sessionKey)
	if err != nil {
		return err
	}
	// The pin stores the bare provider name; custom: is a config selector
	// prefix, not part of the runtime provider identity.
	provider = strings.TrimPrefix(strings.TrimSpace(provider), "custom:")
	model = strings.TrimSpace(model)
	if !validSessionModelField(provider) {
		return errors.New("invalid profile session model provider")
	}
	if !validSessionModelField(model) {
		return errors.New("invalid profile session model name")
	}
	encoded, err := json.Marshal(profileSessionModel{Version: 1, Provider: provider, Model: model})
	if err != nil {
		return fmt.Errorf("encode profile session model pin: %w", err)
	}
	if len(encoded) > sessionModelValueLimit {
		return errors.New("profile session model pin exceeds limit")
	}
	_, remainder, _ := strings.Cut(sessionKey, ":")
	if _, err := s.db.SQL().ExecContext(ctx, `INSERT INTO state_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, sessionModelKey(source, remainder), string(encoded)); err != nil {
		return fmt.Errorf("write profile session model pin: %w", err)
	}
	return nil
}
