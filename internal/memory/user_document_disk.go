package memory

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

const documentDiskSafetyBytes int64 = 256 << 20

// ErrDocumentDiskSpace rejects uploads when safe disk capacity is unavailable.
var ErrDocumentDiskSpace = errors.New("document uploads are temporarily unavailable because safe disk capacity is unavailable; try again later")

// DocumentStorageStats reports physical file sizes and filesystem bytes available
// to this process. DiskKnown is false for memory databases or failed disk probes.
// These are snapshots, not a guarantee against unrelated filesystem growth.
type DocumentStorageStats struct {
	DatabaseBytes int64
	WALBytes      int64
	FreeBytes     int64
	DiskKnown     bool
}

// UserDocumentStorageStats reports database-wide physical usage, not tenant usage.
// Callers must authorize administrative access. Memory databases return zero
// statistics without error; unavailable file-backed statistics return an error.
func (s *Store) UserDocumentStorageStats(ctx context.Context) (stats DocumentStorageStats, err error) {
	started := time.Now()
	defer func() { s.measureDocumentDisk(ctx, "document_storage_stats", started, stats, &err) }()
	return s.userDocumentStorageStats(ctx, s.sql)
}

func (s *Store) userDocumentStorageStats(ctx context.Context, query interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (DocumentStorageStats, error) {
	rows, err := query.QueryContext(ctx, `PRAGMA database_list`)
	if err != nil {
		return DocumentStorageStats{}, err
	}
	defer rows.Close()
	var path string
	found := false
	for rows.Next() {
		var seq int
		var name, filename string
		if err := rows.Scan(&seq, &name, &filename); err != nil {
			return DocumentStorageStats{}, err
		}
		if name == "main" {
			path, found = filename, true
		}
	}
	if err := rows.Err(); err != nil {
		return DocumentStorageStats{}, err
	}
	if !found {
		return DocumentStorageStats{}, ErrDocumentDiskSpace
	}
	if path == "" {
		return DocumentStorageStats{}, nil
	}
	probe := s.documentDiskStat
	if probe == nil {
		probe = statDocumentDisk
	}
	stats, err := probe(path)
	if err != nil || !stats.DiskKnown || stats.FreeBytes < 0 {
		stats.DiskKnown = false
		return stats, ErrDocumentDiskSpace
	}
	return stats, nil
}

func statDocumentDisk(path string) (stats DocumentStorageStats, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return stats, ErrDocumentDiskSpace
	}
	stats.DatabaseBytes = info.Size()
	info, err = os.Stat(path + "-wal")
	if err == nil {
		stats.WALBytes = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return stats, ErrDocumentDiskSpace
	}
	stats.FreeBytes, err = documentFilesystemFreeBytes(path)
	stats.DiskKnown = err == nil
	return stats, err
}

// The caller holds SQLite's immediate write transaction. Global pending source
// and extraction reservations include other owners/handles. Twice the payload
// budgets covers database plus WAL growth; existing file bytes already reduce
// filesystem availability. Unrelated writers can still consume space afterward.
func (s *Store) checkDocumentDiskTx(ctx context.Context, tx *sql.Tx, usage DocumentUsage, sourceBytes int64) (err error) {
	started := time.Now()
	stats, err := s.userDocumentStorageStats(ctx, tx)
	defer func() { s.measureDocumentDisk(ctx, "document_disk_check", started, stats, &err) }()
	if err != nil || !stats.DiskKnown {
		return err
	}
	required := documentDiskSafetyBytes + 2*(sourceBytes+usage.ReservedSourceBytes+usage.ReservedTextBytes)
	if stats.FreeBytes < required {
		return ErrDocumentDiskSpace
	}
	return nil
}

func (s *Store) measureDocumentDisk(ctx context.Context, operation string, started time.Time, stats DocumentStorageStats, err *error) {
	fields := []config.Field{config.F("database_bytes", stats.DatabaseBytes), config.F("wal_bytes", stats.WALBytes), config.F("is_disk_known", stats.DiskKnown)}
	if stats.DiskKnown {
		fields = append(fields, config.F("free_bytes", stats.FreeBytes))
	}
	s.documentMeasured(ctx, operation, started, err, fields...)
}
