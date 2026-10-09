package database

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// ProfileSchemaVersion identifies the approved fresh schema, not an upgrade path.
const ProfileSchemaVersion = 1

//go:embed schema.sql
var profileSchema string

// ErrIncompatibleSchema means that an existing database was left unchanged.
var ErrIncompatibleSchema = errors.New("incompatible profile database schema")

var schemaReference struct {
	sync.Once
	objects []schemaObject
	err     error
}

type schemaObject struct{ Kind, Name, Table, SQL string }

// SQL formatting and dump annotations are not schema differences. Tokenize
// instead of collapsing whitespace: punctuation spacing is irrelevant, while
// whitespace inside a quoted literal is part of its value.
var schemaSQLTokens = regexp.MustCompile(`(?s)--[^\n]*(?:\n|$)|/\*.*?\*/|'(?:''|[^'])*'|"(?:""|[^"])*"|\x60(?:\x60\x60|[^\x60])*\x60|\[[^\]]*\]|[A-Za-z_][A-Za-z_0-9]*|[0-9]+|[^\s]`)

func normalizeSchemaSQL(statement string) string {
	var tokens []string
	for _, token := range schemaSQLTokens.FindAllString(statement, -1) {
		if strings.HasPrefix(token, "--") || strings.HasPrefix(token, "/*") {
			continue
		}
		if token[0] != '\'' && token[0] != '"' && token[0] != '`' && token[0] != '[' {
			token = strings.ToLower(token)
		}
		tokens = append(tokens, token)
	}
	return strings.Join(tokens, " ")
}

func schemaObjects(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]schemaObject, error) {
	rows, err := db.QueryContext(ctx, `SELECT type,name,tbl_name,COALESCE(sql,'') FROM sqlite_schema ORDER BY type,name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var objects []schemaObject
	for rows.Next() {
		var object schemaObject
		if err := rows.Scan(&object.Kind, &object.Name, &object.Table, &object.SQL); err != nil {
			return nil, err
		}
		object.SQL = normalizeSchemaSQL(object.SQL)
		objects = append(objects, object)
	}
	return objects, rows.Err()
}

func expectedObjects() ([]schemaObject, error) {
	schemaReference.Do(func() {
		db, err := sql.Open("sqlite3", ":memory:")
		if err != nil {
			schemaReference.err = err
			return
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		if _, err := db.Exec(profileSchema); err != nil {
			schemaReference.err = err
			return
		}
		schemaReference.objects, schemaReference.err = schemaObjects(context.Background(), db)
	})
	return schemaReference.objects, schemaReference.err
}

// OpenState initializes only an empty profile database. Nonempty incompatible
// schemas are rejected before durability pragmas or canonical mutations.
func OpenState(ctx context.Context, path string, log *config.Logger) (_ *DB, resultErr error) {
	started := time.Now()
	created := false
	initializing := false
	defer func() {
		if log != nil {
			status := "ok"
			if resultErr != nil {
				status = "error"
			}
			log.Server("database").Debug("database.schema.complete", "profile database schema checked", config.F("record_kind", "measurement"), config.F("is_created", created), config.F("duration_ms", time.Since(started).Milliseconds()), config.F("schema_version", ProfileSchemaVersion), config.F("status", status), config.ErrorField(resultErr))
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	directory := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(abs), directory), directory) {
		if part == "" {
			continue
		}
		directory = filepath.Join(directory, part)
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("unsafe profile database directory")
		}
	}
	if info, err := os.Lstat(abs); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("unsafe profile database file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// SQLite opens these sidecars itself. Refuse pre-existing unsafe paths rather
	// than letting an operator-directory symlink redirect database writes.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if info, err := os.Lstat(abs + suffix); err == nil {
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return nil, errors.New("unsafe profile database sidecar")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	file, err := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err == nil {
		if err := file.Close(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: abs}).String() + "?_foreign_keys=on&_secure_delete=on&_busy_timeout=5000&_txlock=immediate"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ok := false
	defer func() {
		if !ok {
			db.Close()
		}
	}()
	schemaInitializationMu.Lock()
	defer schemaInitializationMu.Unlock()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	objects, err := schemaObjects(ctx, tx)
	if err != nil {
		return nil, err
	}
	if len(objects) == 0 {
		if _, err := tx.ExecContext(ctx, profileSchema); err != nil {
			return nil, fmt.Errorf("create profile schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version(version) VALUES (?)`, ProfileSchemaVersion); err != nil {
			return nil, err
		}
		initializing = true
	} else {
		expected, err := expectedObjects()
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(objects, expected) {
			return nil, ErrIncompatibleSchema
		}
		var count, version int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(version),0) FROM schema_version`).Scan(&count, &version); err != nil {
			return nil, err
		}
		if count != 1 || version != ProfileSchemaVersion {
			return nil, ErrIncompatibleSchema
		}
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return nil, err
	}
	hasViolation := rows.Next()
	iterationErr := rows.Err()
	rows.Close()
	if iterationErr != nil {
		return nil, iterationErr
	}
	if hasViolation {
		return nil, ErrIncompatibleSchema
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	created = initializing
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA wal_autocheckpoint=1000`); err != nil {
		return nil, err
	}
	ok = true
	return &DB{path: abs, log: log, db: db}, nil
}
