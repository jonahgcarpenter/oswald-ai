package database

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

var trailingCommaRE = regexp.MustCompile(`,(\s*[}\]])`)

// MigrateLegacyAccountLinks imports the legacy JSON account-link store when the SQLite table is empty.
func (d *DB) MigrateLegacyAccountLinks(path string) error {
	isEmpty, err := d.AccountLinksEmpty()
	if err != nil {
		return err
	}
	if !isEmpty {
		return nil
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to stat legacy account link store: %w", err)
	}

	data, err := d.loadLegacyAccountLinks(path)
	if err != nil {
		return err
	}
	if len(data.Users) == 0 {
		return nil
	}
	if err := d.ReplaceAccountLinks(data); err != nil {
		return fmt.Errorf("failed to migrate account link store: %w", err)
	}

	accountCount := 0
	for _, user := range data.Users {
		accountCount += len(user.Accounts)
	}
	d.log.Info("account_link.store.migrated", "migrated legacy account link store", config.F("source_path", path), config.F("path", d.path), config.F("user_count", len(data.Users)), config.F("account_count", accountCount), config.F("status", "ok"))
	return nil
}

func (d *DB) loadLegacyAccountLinks(path string) (AccountLinkData, error) {
	data := AccountLinkData{
		Version:      1,
		Users:        make(map[string]AccountUser),
		AccountIndex: make(map[string]string),
	}

	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return data, nil
	}
	if err != nil {
		return AccountLinkData{}, fmt.Errorf("failed to read account link store: %w", err)
	}
	if len(raw) == 0 {
		return data, nil
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		sanitized := sanitizeJSON(raw)
		if jsonErr := json.Unmarshal(sanitized, &data); jsonErr != nil {
			backupPath := path + ".corrupt-" + time.Now().UTC().Format("20060102T150405Z")
			if writeErr := os.WriteFile(backupPath, raw, 0o644); writeErr != nil {
				d.log.Warn("account_link.store.backup_failed", "failed to back up corrupt account link store", config.F("path", backupPath), config.F("status", "degraded"), config.ErrorField(writeErr))
			}
			return AccountLinkData{}, fmt.Errorf("failed to decode account link store: %w", err)
		}
		if err := os.WriteFile(path, sanitized, 0o644); err != nil {
			d.log.Warn("account_link.store.recovered", "recovered malformed JSON in memory but could not rewrite store", config.F("path", path), config.F("status", "degraded"), config.ErrorField(err))
		} else {
			d.log.Warn("account_link.store.repaired", "repaired malformed account link store", config.F("path", path), config.F("status", "degraded"))
		}
	}
	if data.Users == nil {
		data.Users = make(map[string]AccountUser)
	}
	if data.AccountIndex == nil {
		data.AccountIndex = make(map[string]string)
	}
	if data.Version == 0 {
		data.Version = 1
	}
	return data, nil
}

func sanitizeJSON(raw []byte) []byte {
	trimmed := []byte(strings.TrimSpace(string(raw)))
	if len(trimmed) == 0 {
		return raw
	}
	return trailingCommaRE.ReplaceAll(trimmed, []byte("$1"))
}
