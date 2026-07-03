package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
)

// Store persists encrypted MCP server configurations.
type Store struct {
	db       *database.DB
	crypto   *cryptoBox
	resolver hostnameResolver
	log      *config.Logger
}

// NewStore opens the shared SQLite database and prepares MCP config encryption.
func NewStore(path string, encryptionKey string, log *config.Logger) (*Store, error) {
	box, err := newCryptoBox(encryptionKey)
	if err != nil {
		return nil, err
	}
	db, err := database.Open(path, log)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, crypto: box, log: log}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) SetResolverForTest(resolver hostnameResolver) {
	s.resolver = resolver
}

// Save validates, encrypts, and upserts one MCP server config.
func (s *Store) Save(ctx context.Context, cfg ServerConfig) (ServerConfig, error) {
	if s == nil || s.db == nil {
		return ServerConfig{}, fmt.Errorf("MCP store is not initialized")
	}
	cfg.Scope = strings.TrimSpace(cfg.Scope)
	cfg.OwnerUserID = strings.TrimSpace(cfg.OwnerUserID)
	cfg.Name = strings.TrimSpace(strings.ToLower(cfg.Name))
	cfg.Type = strings.TrimSpace(strings.ToLower(cfg.Type))
	if cfg.Type == "" {
		cfg.Type = "generic"
	}
	cfg.Transport = strings.TrimSpace(strings.ToLower(cfg.Transport))
	if cfg.Transport == "" {
		cfg.Transport = TransportStreamableHTTP
	}
	if err := validateScope(cfg.Scope, cfg.OwnerUserID); err != nil {
		return ServerConfig{}, err
	}
	if err := validateServerName(cfg.Name); err != nil {
		return ServerConfig{}, err
	}
	if err := validateTransport(cfg.Transport); err != nil {
		return ServerConfig{}, err
	}
	parsed, err := parseAndValidateURL(ctx, cfg.URL, s.resolver)
	if err != nil {
		return ServerConfig{}, err
	}
	if cfg.Scope == ScopeUser {
		if _, ok, err := s.Get(ctx, ScopeGlobal, "", cfg.Name); err != nil {
			return ServerConfig{}, err
		} else if ok {
			return ServerConfig{}, fmt.Errorf("server name %q collides with a global MCP server", cfg.Name)
		}
	}
	if existing, ok, err := s.Get(ctx, cfg.Scope, cfg.OwnerUserID, cfg.Name); err != nil {
		return ServerConfig{}, err
	} else if ok {
		cfg.ID = existing.ID
		cfg.CreatedAt = existing.CreatedAt
	}
	if cfg.ID == "" {
		cfg.ID = newConfigID()
	}
	now := time.Now().UTC()
	if cfg.CreatedAt.IsZero() {
		cfg.CreatedAt = now
	}
	cfg.UpdatedAt = now
	urlCiphertext, err := s.crypto.encrypt(strings.TrimSpace(cfg.URL), fieldAAD(cfg.Scope, cfg.OwnerUserID, cfg.Name, "url"))
	if err != nil {
		return ServerConfig{}, err
	}
	headersData, err := json.Marshal(cfg.Headers)
	if err != nil {
		return ServerConfig{}, fmt.Errorf("marshal MCP headers: %w", err)
	}
	headersCiphertext, err := s.crypto.encrypt(string(headersData), fieldAAD(cfg.Scope, cfg.OwnerUserID, cfg.Name, "headers"))
	if err != nil {
		return ServerConfig{}, err
	}
	_, err = s.db.SQL().ExecContext(ctx, `
INSERT INTO mcp_servers (id, scope, owner_user_id, name, type, transport, url_ciphertext, url_host_hash, headers_ciphertext, enabled, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	scope = excluded.scope,
	owner_user_id = excluded.owner_user_id,
	name = excluded.name,
	type = excluded.type,
	transport = excluded.transport,
	url_ciphertext = excluded.url_ciphertext,
	url_host_hash = excluded.url_host_hash,
	headers_ciphertext = excluded.headers_ciphertext,
	enabled = excluded.enabled,
	updated_at = excluded.updated_at
`, cfg.ID, cfg.Scope, nullableOwner(cfg.OwnerUserID), cfg.Name, cfg.Type, cfg.Transport, urlCiphertext, s.crypto.hostHash(parsed.Hostname()), headersCiphertext, boolToInt(cfg.Enabled), formatTime(cfg.CreatedAt), formatTime(cfg.UpdatedAt))
	if err != nil {
		return ServerConfig{}, fmt.Errorf("save MCP server config: %w", err)
	}
	return cfg, nil
}

func (s *Store) ListForUser(ctx context.Context, userID string) ([]ServerConfig, error) {
	rows, err := s.db.SQL().QueryContext(ctx, `
SELECT id, scope, owner_user_id, name, type, transport, url_ciphertext, url_host_hash, headers_ciphertext, enabled, created_at, updated_at
FROM mcp_servers
WHERE scope = 'global' OR (scope = 'user' AND owner_user_id = ?)
ORDER BY scope, name
`, strings.TrimSpace(userID))
	if err != nil {
		return nil, fmt.Errorf("list MCP server configs: %w", err)
	}
	defer rows.Close()
	return s.scanConfigs(rows)
}

func (s *Store) ListGlobal(ctx context.Context) ([]ServerConfig, error) {
	rows, err := s.db.SQL().QueryContext(ctx, `
SELECT id, scope, owner_user_id, name, type, transport, url_ciphertext, url_host_hash, headers_ciphertext, enabled, created_at, updated_at
FROM mcp_servers
WHERE scope = 'global'
ORDER BY name
`)
	if err != nil {
		return nil, fmt.Errorf("list global MCP server configs: %w", err)
	}
	defer rows.Close()
	return s.scanConfigs(rows)
}

func (s *Store) Get(ctx context.Context, scope, ownerUserID, name string) (ServerConfig, bool, error) {
	row := s.db.SQL().QueryRowContext(ctx, `
SELECT id, scope, owner_user_id, name, type, transport, url_ciphertext, url_host_hash, headers_ciphertext, enabled, created_at, updated_at
FROM mcp_servers
WHERE scope = ? AND COALESCE(owner_user_id, '') = ? AND name = ?
`, scope, strings.TrimSpace(ownerUserID), strings.TrimSpace(strings.ToLower(name)))
	stored, err := scanStored(row)
	if err == sql.ErrNoRows {
		return ServerConfig{}, false, nil
	}
	if err != nil {
		return ServerConfig{}, false, err
	}
	cfg, err := s.decrypt(stored)
	return cfg, err == nil, err
}

func (s *Store) Delete(ctx context.Context, scope, ownerUserID, name string) error {
	_, err := s.db.SQL().ExecContext(ctx, `DELETE FROM mcp_servers WHERE scope = ? AND COALESCE(owner_user_id, '') = ? AND name = ?`, scope, strings.TrimSpace(ownerUserID), strings.TrimSpace(strings.ToLower(name)))
	if err != nil {
		return fmt.Errorf("delete MCP server config: %w", err)
	}
	return nil
}

func (s *Store) SetEnabled(ctx context.Context, scope, ownerUserID, name string, enabled bool) error {
	_, err := s.db.SQL().ExecContext(ctx, `UPDATE mcp_servers SET enabled = ?, updated_at = ? WHERE scope = ? AND COALESCE(owner_user_id, '') = ? AND name = ?`, boolToInt(enabled), formatTime(time.Now().UTC()), scope, strings.TrimSpace(ownerUserID), strings.TrimSpace(strings.ToLower(name)))
	if err != nil {
		return fmt.Errorf("update MCP server enabled state: %w", err)
	}
	return nil
}

func (s *Store) scanConfigs(rows *sql.Rows) ([]ServerConfig, error) {
	var out []ServerConfig
	for rows.Next() {
		stored, err := scanStored(rows)
		if err != nil {
			return nil, err
		}
		cfg, err := s.decrypt(stored)
		if err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan MCP server configs: %w", err)
	}
	return out, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanStored(row rowScanner) (storedServerConfig, error) {
	var stored storedServerConfig
	var owner sql.NullString
	var enabled int
	var createdRaw, updatedRaw string
	if err := row.Scan(&stored.ID, &stored.Scope, &owner, &stored.Name, &stored.Type, &stored.Transport, &stored.URLCiphertext, &stored.URLHostHash, &stored.HeadersCiphertext, &enabled, &createdRaw, &updatedRaw); err != nil {
		return storedServerConfig{}, err
	}
	stored.OwnerUserID = owner.String
	stored.Enabled = enabled != 0
	createdAt, err := time.Parse(time.RFC3339Nano, createdRaw)
	if err != nil {
		return storedServerConfig{}, fmt.Errorf("parse MCP server created_at: %w", err)
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, updatedRaw)
	if err != nil {
		return storedServerConfig{}, fmt.Errorf("parse MCP server updated_at: %w", err)
	}
	stored.CreatedAt = createdAt
	stored.UpdatedAt = updatedAt
	return stored, nil
}

func (s *Store) decrypt(stored storedServerConfig) (ServerConfig, error) {
	urlText, err := s.crypto.decrypt(stored.URLCiphertext, fieldAAD(stored.Scope, stored.OwnerUserID, stored.Name, "url"))
	if err != nil {
		return ServerConfig{}, err
	}
	headersText, err := s.crypto.decrypt(stored.HeadersCiphertext, fieldAAD(stored.Scope, stored.OwnerUserID, stored.Name, "headers"))
	if err != nil {
		return ServerConfig{}, err
	}
	headers := map[string]string{}
	if strings.TrimSpace(headersText) != "" && strings.TrimSpace(headersText) != "null" {
		if err := json.Unmarshal([]byte(headersText), &headers); err != nil {
			return ServerConfig{}, fmt.Errorf("unmarshal MCP headers: %w", err)
		}
	}
	return ServerConfig{ID: stored.ID, Scope: stored.Scope, OwnerUserID: stored.OwnerUserID, Name: stored.Name, Type: stored.Type, Transport: stored.Transport, URL: urlText, Headers: headers, Enabled: stored.Enabled, CreatedAt: stored.CreatedAt, UpdatedAt: stored.UpdatedAt}, nil
}

func nullableOwner(owner string) any {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil
	}
	return owner
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func newConfigID() string {
	return fmt.Sprintf("mcp_%d", time.Now().UTC().UnixNano())
}
