package database

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

var legacyExplicitEvidenceDateRE = regexp.MustCompile(`(?i)\bDate:\s*\[[^\]]+\]$`)

// MigrateLegacyUserMemory imports legacy per-user Markdown files into SQLite.
// Source files are intentionally left untouched as backups.
func (d *DB) MigrateLegacyUserMemory(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read legacy user memory directory: %w", err)
	}

	imported := 0
	skipped := 0
	failed := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		userID := strings.TrimSuffix(entry.Name(), ".md")
		if userID == "" {
			skipped++
			continue
		}

		filePath := filepath.Join(path, entry.Name())
		raw, err := os.ReadFile(filePath)
		if err != nil {
			failed++
			d.log.Warn("memory.user.legacy.read_failed", "failed to read legacy user memory", config.F("path", filePath), config.F("status", "degraded"), config.ErrorField(err))
			continue
		}

		parsed := parseLegacyUserMemory(migrateLegacyFlatMemoryIfNeeded(string(raw)))
		if strings.TrimSpace(parsed.Intro) == "" && len(parsed.Sections) == 0 {
			skipped++
			continue
		}

		inserted, err := d.importLegacyUserMemory(userID, parsed)
		if err != nil {
			failed++
			d.log.Warn("memory.user.legacy.import_failed", "failed to import legacy user memory", config.F("path", filePath), config.F("user_id", userID), config.F("status", "degraded"), config.ErrorField(err))
			continue
		}
		if inserted == 0 {
			skipped++
			continue
		}
		imported++
	}

	d.log.Info("memory.user.legacy.migrated", "migrated legacy user memory markdown", config.F("source_path", path), config.F("imported_count", imported), config.F("skipped_count", skipped), config.F("failed_count", failed), config.F("status", "ok"))
	if failed > 0 {
		return fmt.Errorf("failed to import %d legacy user memory files", failed)
	}
	return nil
}

func (d *DB) importLegacyUserMemory(userID string, parsed legacyUserMemory) (int, error) {
	now := formatDBTime(time.Now().UTC())
	tx, err := d.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("failed to begin legacy user memory migration: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck

	if _, err := tx.Exec(`INSERT OR IGNORE INTO account_users (canonical_user_id, created_at, updated_at) VALUES (?, ?, ?)`, userID, now, now); err != nil {
		return 0, fmt.Errorf("failed to ensure account user for legacy memory: %w", err)
	}

	if strings.TrimSpace(parsed.Intro) != "" {
		if _, err := tx.Exec(`
INSERT INTO user_memory_profiles (canonical_user_id, intro, created_at, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(canonical_user_id) DO UPDATE SET
	intro = CASE WHEN trim(user_memory_profiles.intro) = '' THEN excluded.intro ELSE user_memory_profiles.intro END,
	updated_at = excluded.updated_at
`, userID, strings.TrimSpace(parsed.Intro), now, now); err != nil {
			return 0, fmt.Errorf("failed to import legacy user memory profile: %w", err)
		}
	}

	stmt, err := tx.Prepare(`
INSERT OR IGNORE INTO user_memory_entries (canonical_user_id, category, statement, statement_key, evidence, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
`)
	if err != nil {
		return 0, fmt.Errorf("failed to prepare legacy user memory import: %w", err)
	}
	defer stmt.Close()

	inserted := 0
	for _, category := range legacyUserMemoryCategories {
		for _, entry := range parsed.Sections[category] {
			res, err := stmt.Exec(userID, category, entry.Statement, legacyStatementKey(entry.Statement), entry.Evidence, now, now)
			if err != nil {
				return 0, fmt.Errorf("failed to import legacy user memory entry: %w", err)
			}
			changed, err := res.RowsAffected()
			if err != nil {
				return 0, fmt.Errorf("failed to inspect legacy user memory import result: %w", err)
			}
			if changed > 0 {
				inserted++
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit legacy user memory migration: %w", err)
	}
	return inserted, nil
}

var legacyUserMemoryCategories = []string{"identity", "system_rules", "preferences", "notes"}

type legacyUserMemory struct {
	Intro    string
	Sections map[string][]legacyUserMemoryEntry
}

type legacyUserMemoryEntry struct {
	Statement string
	Evidence  string
}

func parseLegacyUserMemory(content string) legacyUserMemory {
	parsed := legacyUserMemory{Sections: make(map[string][]legacyUserMemoryEntry)}
	body := legacyMemoryBody(content)

	var introLines []string
	var currentCat string
	var buf strings.Builder

	flush := func() {
		if currentCat == "" {
			return
		}
		parsed.Sections[currentCat] = append(parsed.Sections[currentCat], parseLegacyEntries(buf.String())...)
		buf.Reset()
	}

	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "## ") {
			if currentCat == "" {
				parsed.Intro = strings.TrimSpace(strings.Join(introLines, "\n"))
			} else {
				flush()
			}
			currentCat = normalizeLegacyCategory(strings.TrimPrefix(line, "## "))
			continue
		}
		if currentCat == "" {
			introLines = append(introLines, line)
			continue
		}
		buf.WriteString(line + "\n")
	}
	if currentCat == "" {
		parsed.Intro = strings.TrimSpace(strings.Join(introLines, "\n"))
	} else {
		flush()
	}
	return parsed
}

func parseLegacyEntries(body string) []legacyUserMemoryEntry {
	raw := strings.Split(strings.TrimSpace(body), "\n\n")
	entries := make([]legacyUserMemoryEntry, 0)
	for i := 0; i < len(raw); i++ {
		block := strings.TrimSpace(raw[i])
		if block == "" {
			continue
		}
		lines := strings.SplitN(block, "\n", 2)
		if len(lines) == 2 && strings.HasPrefix(strings.TrimSpace(lines[1]), "- Evidence:") {
			entries = append(entries, legacyEntryFromBlock(block))
			continue
		}
		if i+1 < len(raw) && strings.HasPrefix(strings.TrimSpace(raw[i+1]), "- Evidence:") {
			entries = append(entries, legacyEntryFromBlock(block+"\n"+strings.TrimSpace(raw[i+1])))
			i++
		}
	}
	return entries
}

func legacyEntryFromBlock(block string) legacyUserMemoryEntry {
	lines := strings.SplitN(strings.TrimSpace(block), "\n", 2)
	entry := legacyUserMemoryEntry{Statement: strings.TrimSpace(lines[0])}
	if len(lines) == 2 {
		evidence := strings.TrimSpace(lines[1])
		evidence = strings.TrimPrefix(evidence, "- Evidence:")
		entry.Evidence = sanitizeLegacyEvidence(evidence)
	}
	return entry
}

func migrateLegacyFlatMemoryIfNeeded(content string) string {
	if content == "" || strings.Contains(content, "\n## ") {
		return content
	}
	entries := parseLegacyEntries(legacyMemoryBody(content))
	if len(entries) == 0 {
		return "# User Memory\n"
	}
	var sb strings.Builder
	sb.WriteString("# User Memory\n\n## Notes\n")
	for _, entry := range entries {
		sb.WriteString("\n")
		sb.WriteString(entry.Statement)
		sb.WriteString("\n\n- Evidence: ")
		sb.WriteString(entry.Evidence)
		if !legacyExplicitEvidenceDateRE.MatchString(entry.Evidence) {
			sb.WriteString(".")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func legacyMemoryBody(content string) string {
	body := content
	if strings.HasPrefix(content, "# User Memory") {
		idx := strings.Index(content, "\n")
		if idx >= 0 {
			body = content[idx+1:]
		} else {
			body = ""
		}
	}
	return strings.TrimLeft(body, "\n")
}

func normalizeLegacyCategory(category string) string {
	category = strings.TrimSpace(strings.ToLower(category))
	category = strings.ReplaceAll(category, " ", "_")
	for _, valid := range legacyUserMemoryCategories {
		if category == valid {
			return category
		}
	}
	return "notes"
}

func legacyStatementKey(statement string) string {
	return strings.ToLower(strings.TrimSpace(statement))
}

func sanitizeLegacyEvidence(evidence string) string {
	evidence = strings.TrimSpace(evidence)
	evidence = strings.TrimRight(evidence, ". ")
	return evidence
}
