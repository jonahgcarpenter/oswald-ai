package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestUserDocumentStorageStats(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native disk statistics require Linux")
	}
	path := filepath.Join(t.TempDir(), "private-path-canary.db")
	s := newTestStore(path, nil)
	t.Cleanup(func() { _ = s.Close() })
	stats, err := s.UserDocumentStorageStats(context.Background())
	if err != nil || !stats.DiskKnown || stats.FreeBytes <= 0 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	for filename, size := range map[string]int64{path: stats.DatabaseBytes, path + "-wal": stats.WALBytes} {
		info, err := os.Stat(filename)
		if err != nil || info.Size() != size {
			t.Fatalf("physical file size mismatch: %v", err)
		}
	}
	s.documentDiskStat = func(actual string) (DocumentStorageStats, error) {
		if actual != path {
			t.Fatal("database path did not come from this store")
		}
		return DocumentStorageStats{}, errors.New("private-path-canary")
	}
	stats, err = s.UserDocumentStorageStats(context.Background())
	if !errors.Is(err, ErrDocumentDiskSpace) || stats.DiskKnown || strings.Contains(err.Error(), "canary") {
		t.Fatalf("unsafe stat failure: %+v %v", stats, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.UserDocumentStorageStats(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestUserDocumentDiskMemoryDatabase(t *testing.T) {
	s := newTestStore(":memory:", nil)
	t.Cleanup(func() { _ = s.Close() })
	seedAccountUsers(t, s, "user")
	s.documentDiskStat = func(string) (DocumentStorageStats, error) {
		t.Fatal("memory database must not probe disk")
		return DocumentStorageStats{}, ErrDocumentDiskSpace
	}
	stats, err := s.UserDocumentStorageStats(context.Background())
	if err != nil || stats != (DocumentStorageStats{}) {
		t.Fatalf("memory stats: %+v %v", stats, err)
	}
	r := reserveDocumentUpload(t, s, "user", 1, 100)
	if _, err := s.AcceptReservedUserDocuments(context.Background(), "user", r.ID, []DocumentUpload{documentUpload("")}); err != nil {
		t.Fatal(err)
	}
	acceptDocument(t, s, "user")
}

func TestUserDocumentDiskAdmissionBeforeBlob(t *testing.T) {
	for _, failure := range []string{"low", "unknown", "stat_error"} {
		for _, reserved := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reserved=%v", failure, reserved), func(t *testing.T) {
				s := newFormationTestStore(t)
				s.documentDiskStat = func(string) (DocumentStorageStats, error) {
					return DocumentStorageStats{DiskKnown: true, FreeBytes: 1 << 30}, nil
				}
				var id string
				if reserved {
					id = reserveDocumentUpload(t, s, "user", 1, 100).ID
				}
				s.documentDiskStat = func(string) (DocumentStorageStats, error) {
					stats := DocumentStorageStats{DiskKnown: failure != "unknown", FreeBytes: documentDiskSafetyBytes}
					if failure == "stat_error" {
						return stats, errors.New("private disk error canary")
					}
					return stats, nil
				}
				if _, err := s.sql.Exec(`CREATE TRIGGER forbid_disk_blob BEFORE INSERT ON user_document_sources BEGIN SELECT RAISE(ABORT,'BLOB insert attempted'); END`); err != nil {
					t.Fatal(err)
				}
				var err error
				if reserved {
					_, err = s.AcceptReservedUserDocuments(context.Background(), "user", id, []DocumentUpload{documentUpload("")})
				} else {
					_, err = s.AcceptUserDocuments(context.Background(), "user", []DocumentUpload{documentUpload("")})
				}
				if !errors.Is(err, ErrDocumentDiskSpace) {
					t.Fatalf("accept: %v", err)
				}
				if _, err := s.ReserveUserDocumentUpload(context.Background(), "user", 1, 100); !errors.Is(err, ErrDocumentDiskSpace) {
					t.Fatalf("reserve: %v", err)
				}
				for _, table := range []string{"user_documents", "user_document_sources", "user_document_extraction_jobs", "user_document_reservations"} {
					assertStoreCount(t, s.sql, `SELECT count(*) FROM `+table, 0)
				}
			})
		}
	}
}

func TestUserDocumentDiskGlobalReservations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	s := newTestStore(path, nil)
	t.Cleanup(func() { _ = s.Close() })
	other := newTestStore(path, nil)
	t.Cleanup(func() { _ = other.Close() })
	seedAccountUsers(t, s, "user", "other")
	// Exactly enough for one reservation, across both canonical owners/handles.
	probe := func(string) (DocumentStorageStats, error) {
		return DocumentStorageStats{DiskKnown: true, FreeBytes: documentDiskSafetyBytes + 2*(100+DocumentMaxTextBytes)}, nil
	}
	s.documentDiskStat, other.documentDiskStat = probe, probe
	var wg sync.WaitGroup
	results := make(chan error, 2)
	reservations := make(chan DocumentReservation, 2)
	start := make(chan struct{})
	for owner, store := range map[string]*Store{"user": s, "other": other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := store.ReserveUserDocumentUpload(context.Background(), owner, 1, 100)
			results <- err
			if err == nil {
				reservations <- r
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	accepted, rejected := 0, 0
	for err := range results {
		if err == nil {
			accepted++
		} else if errors.Is(err, ErrDocumentDiskSpace) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("accepted=%d rejected=%d", accepted, rejected)
	}
	if _, err := other.AcceptUserDocuments(context.Background(), "other", []DocumentUpload{documentUpload("")}); !errors.Is(err, ErrDocumentDiskSpace) {
		t.Fatalf("direct admission ignored global reservation: %v", err)
	}
	r := <-reservations
	if _, err := s.AcceptReservedUserDocuments(context.Background(), r.UserID, r.ID, []DocumentUpload{documentUpload("replay")}); err != nil {
		t.Fatalf("own reservation double charged: %v", err)
	}
	// Existing queued extraction still consumes headroom after source admission.
	if _, err := s.ReserveUserDocumentUpload(context.Background(), "user", 1, 100); !errors.Is(err, ErrDocumentDiskSpace) {
		t.Fatalf("queued extraction not budgeted: %v", err)
	}
	s.documentDiskStat = func(string) (DocumentStorageStats, error) { return DocumentStorageStats{}, ErrDocumentDiskSpace }
	if _, err := s.AcceptUserDocuments(context.Background(), r.UserID, []DocumentUpload{documentUpload("replay")}); err != nil {
		t.Fatalf("idempotent replay required new disk capacity: %v", err)
	}
}

func TestUserDocumentDiskHeadroomAndReleasedReservations(t *testing.T) {
	s := newFormationTestStore(t)
	ctx := context.Background()
	const sourceBytes int64 = 100
	free := documentDiskSafetyBytes + 2*(sourceBytes+DocumentMaxTextBytes) - 1
	s.documentDiskStat = func(string) (DocumentStorageStats, error) {
		return DocumentStorageStats{DiskKnown: true, FreeBytes: free}, nil
	}
	if _, err := s.ReserveUserDocumentUpload(ctx, "user", 1, sourceBytes); !errors.Is(err, ErrDocumentDiskSpace) {
		t.Fatalf("accepted one byte below headroom: %v", err)
	}
	free++
	r := reserveDocumentUpload(t, s, "user", 1, sourceBytes)
	if err := s.ReleaseUserDocumentUpload(ctx, "user", r.ID); err != nil {
		t.Fatal(err)
	}
	r = reserveDocumentUpload(t, s, "user", 1, sourceBytes)
	expireDocumentReservation(t, s, r.ID)
	r = reserveDocumentUpload(t, s, "user", 1, sourceBytes)
	if err := s.ReleaseUserDocumentUpload(ctx, "user", r.ID); err != nil {
		t.Fatal(err)
	}
	u := documentUpload("")
	u.Data = bytes.Repeat([]byte("x"), int(sourceBytes))
	free--
	if _, err := s.AcceptUserDocuments(ctx, "user", []DocumentUpload{u}); !errors.Is(err, ErrDocumentDiskSpace) {
		t.Fatalf("direct admission ignored headroom: %v", err)
	}
	free++
	if _, err := s.AcceptUserDocuments(ctx, "user", []DocumentUpload{u}); err != nil {
		t.Fatalf("direct boundary admission: %v", err)
	}
}

func TestUserDocumentDiskTelemetry(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			s := newFormationTestStore(t)
			var output bytes.Buffer
			s.log = config.NewLogger(level)
			s.log.SetOutput(&output)
			s.documentDiskStat = func(string) (DocumentStorageStats, error) {
				return DocumentStorageStats{DatabaseBytes: 123, WALBytes: 456, DiskKnown: true, FreeBytes: 789}, nil
			}
			_, _ = s.ReserveUserDocumentUpload(context.Background(), "user", 1, 100)
			_, _ = s.UserDocumentStorageStats(context.Background())
			decoder := json.NewDecoder(&output)
			operations := map[string]int{}
			for decoder.More() {
				var record map[string]any
				if err := decoder.Decode(&record); err != nil {
					t.Fatal(err)
				}
				operation, _ := record["operation"].(string)
				operations[operation]++
				if record["level"] != "info" {
					t.Fatalf("missing INFO: %v", record)
				}
				if operation != "document_storage_stats" && (record["is_disk_space_rejected"] != true || record["status"] != "rejected") {
					t.Fatalf("missing rejection: %v", record)
				}
				if operation != "document_reserve" && (record["database_bytes"] != float64(123) || record["wal_bytes"] != float64(456) || record["free_bytes"] != float64(789) || record["is_disk_known"] != true) {
					t.Fatalf("missing numeric stats: %v", record)
				}
			}
			if operations["document_reserve"] != 1 || operations["document_disk_check"] != 1 || operations["document_storage_stats"] != 1 {
				t.Fatalf("measurement counts: %v", operations)
			}
			output.Reset()
			s.documentDiskStat = func(string) (DocumentStorageStats, error) {
				return DocumentStorageStats{}, errors.New("private-path-canary")
			}
			_, _ = s.UserDocumentStorageStats(context.Background())
			if strings.Contains(output.String(), "canary") || strings.Contains(output.String(), `"free_bytes"`) || !strings.Contains(output.String(), `"is_disk_known":false`) {
				t.Fatalf("unknown/private disk information logged: %s", output.String())
			}
		})
	}
}
