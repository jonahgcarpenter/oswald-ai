package memory

import (
	"context"
	"errors"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/imagecache"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ProfileStore owns one manually provisioned profile's approved state.db. It
// never resolves accounts or creates profiles, and cannot serve another owner.
type ProfileStore struct {
	db      *database.DB
	profile string
	log     *config.Logger
	now     func() time.Time
	cache   *imagecache.Cache
}

type profileSessionOrigin struct {
	Version    int     `json:"version"`
	Generation int     `json:"generation"`
	TTLSeconds float64 `json:"ttl_seconds"`
}

type profileFileSnapshot struct {
	Version int    `json:"version"`
	User    string `json:"user"`
	Memory  string `json:"memory"`
}

// profileExchangeState uses one bounded state_meta value per exchange. Message
// rows hold the text; assistant message IDs are stable source-turn identifiers.
type profileExchangeState struct {
	Version       int                   `json:"version"`
	UserMessageID int64                 `json:"user_message_id"`
	History       string                `json:"history"`
	ToolNames     []string              `json:"tool_names"`
	Pressure      SessionPromptPressure `json:"pressure"`
	Delivery      string                `json:"delivery"`
	Images        []profileImage        `json:"images,omitempty"`
}

// NewProfileStore opens state.db below an existing, trusted profile directory.
func logStateRepair(profile, artifact string, before os.FileMode) {
	config.NewLogger(config.LevelWarn).Server("memory").Warn("state.perms.repaired", "repaired private permissions", config.F("config_source", artifact), config.F("old_mode", int(before.Perm())), config.F("profile", profile))
}

func repairStateDir(path, profile string) {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	if info.Mode().Perm()&0077 == 0 {
		return
	}
	before := info.Mode()
	if err := os.Chmod(path, 0700); err != nil {
		return
	}
	logStateRepair(profile, "state_dir", before)
}

func repairStateFile(path, profile, artifact string) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	if info.Mode().Perm()&0077 == 0 {
		return
	}
	before := info.Mode()
	if err := os.Chmod(path, 0600); err != nil {
		return
	}
	logStateRepair(profile, artifact, before)
}

func NewProfileStore(ctx context.Context, root, profile string, log *config.Logger) (*ProfileStore, error) {
	if profile != "default" && !config.ValidProfileName(profile) {
		return nil, errors.New("invalid profile store owner")
	}
	repairStateDir(root, profile)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("profile state directory is not private")
	}
	repairStateFile(filepath.Join(root, "state.db"), profile, "state_db")
	repairStateFile(filepath.Join(root, "state.db-wal"), profile, "state_db_wal")
	repairStateFile(filepath.Join(root, "state.db-shm"), profile, "state_db_shm")
	repairStateFile(filepath.Join(root, "state.db-journal"), profile, "state_db_journal")
	db, err := database.OpenState(ctx, filepath.Join(root, "state.db"), log)
	if err != nil {
		return nil, err
	}
	var incompatible int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE profile_name IS NULL OR profile_name<>?`, profile).Scan(&incompatible); err != nil {
		db.Close()
		return nil, err
	}
	if incompatible != 0 {
		db.Close()
		return nil, errors.New("profile database ownership mismatch")
	}
	return &ProfileStore{db: db, profile: profile, log: log, now: time.Now, cache: imagecache.NewProfileCache(root, profile)}, nil
}

// Close releases this profile's database handle after its workers have stopped.
func (s *ProfileStore) Close() error { return s.db.Close() }

func (s *ProfileStore) measure(event string, started time.Time, err *error) {
	if s.log == nil {
		return
	}
	status := "ok"
	outcome := "completed"
	if *err != nil {
		status = "error"
		outcome = "failed"
		if errors.Is(*err, context.Canceled) {
			status = "rejected"
			outcome = "canceled"
		}
	}
	fields := []config.Field{config.F("profile", s.profile), config.F("duration_ms", time.Since(started).Milliseconds()), config.F("status", status), config.F("outcome", outcome), config.ErrorField(*err)}
	log := s.log.Server("memory.profile")
	switch event {
	case "memory.profile.session.complete":
		log.Debug("memory.profile.session.complete", "resolved profile session", fields...)
	case "memory.profile.reset.complete":
		log.Debug("memory.profile.reset.complete", "reset profile session", fields...)
	case "memory.profile.snapshot.complete":
		log.Debug("memory.profile.snapshot.complete", "bound profile file snapshot", fields...)
	case "memory.profile.turn.complete":
		log.Debug("memory.profile.turn.complete", "stored pending profile exchange", fields...)
	case "memory.profile.delivery.complete":
		log.Debug("memory.profile.delivery.complete", "recorded profile exchange delivery", fields...)
	case "memory.profile.delegation.complete":
		log.Debug("memory.profile.delegation.complete", "recorded delegation lifecycle transition", fields...)
	case "memory.profile.delegation.delivery":
		log.Debug("memory.profile.delegation.delivery", "recorded delegation handoff", fields...)
	}
}

func (s *ProfileStore) scope(owner, key string) (string, error) {
	if owner != s.profile || strings.TrimSpace(key) != key || key == "" || len(key) > 1024 {
		return "", errors.New("invalid profile session scope")
	}
	source, _, ok := strings.Cut(key, ":")
	if !ok || (source != "discord" && source != "imessage" && source != "cron") {
		return "", errors.New("invalid profile session source")
	}
	return source, nil
}

func snapshotKey(id string) string { return "oswald:v1:files:" + id }
func exchangeKey(id string, turn int64) string {
	return "oswald:v1:turn:" + id + ":" + strconv.FormatInt(turn, 10)
}
