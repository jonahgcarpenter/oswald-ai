package config

import (
	"os"
	"path/filepath"
	"strings"
)

// ProfileRoute binds a transport-authenticated identity to an operator profile.

func logPermRepair(source string, before os.FileMode, profile string) {
	fields := []Field{F("config_source", source), F("old_mode", int(before.Perm()))}
	if profile != "" {
		fields = append(fields, F("profile", profile))
	}
	NewLogger(LevelWarn).Server("config").Warn("config.perms.repaired", "repaired private permissions", fields...)
}

func repairPrivateDir(abs, source, profile string) {
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	if info.Mode().Perm()&0077 == 0 {
		return
	}
	before := info.Mode()
	if err := os.Chmod(abs, 0700); err != nil {
		return
	}
	logPermRepair(source, before, profile)
}

func strictPrivateDirectory(path, source, profile string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return withConfigSource(configErr("config_dir_unavailable", ""), source)
	}
	_ = profile
	current := string(os.PathSeparator)
	for _, part := range strings.Split(strings.TrimPrefix(abs, current), current) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return withConfigSource(configErr("config_dir_unavailable", ""), source)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (current == abs && info.Mode().Perm()&0077 != 0) {
			return withConfigSource(configErr("config_dir_unsafe", ""), source)
		}
	}
	return nil
}

func privateDirectory(path, source, profile string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return withConfigSource(configErr("config_dir_unavailable", ""), source)
	}
	repairPrivateDir(abs, source, profile)
	current := string(os.PathSeparator)
	for _, part := range strings.Split(strings.TrimPrefix(abs, current), current) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return withConfigSource(configErr("config_dir_unavailable", ""), source)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (current == abs && info.Mode().Perm()&0077 != 0) {
			return withConfigSource(configErr("config_dir_unsafe", ""), source)
		}
	}
	return nil
}

// knownPlatforms bounds configured platform names before any validation.
var knownPlatforms = map[string]bool{"discord": true, "bluebubbles": true, "api": true}

var platformTopKeys = map[string]bool{"enabled": true, "extra": true, "channel_overrides": true}

var platformExtraKeys = map[string]bool{
	"token": true, "allowed_users": true, "banned_users": true,
	"require_mention": true,
	"server_url":      true, "server_password": true,
	"webhook_host": true, "webhook_callback_host": true, "webhook_port": true, "webhook_path": true, "mention_pattern": true,
	"api_host": true, "api_port": true,
}
