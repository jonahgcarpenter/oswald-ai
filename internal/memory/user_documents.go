package memory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

// Document limits are UTF-8/source bytes, not runes or tokens. Reservations count
// toward extracted quotas until extraction reaches a terminal state.
const (
	DocumentMaxSourceBytes       = 20 << 20
	DocumentMaxRequestBytes      = 40 << 20
	DocumentMaxRequestFiles      = 4
	DocumentMaxUserCount         = 50
	DocumentMaxUserSourceBytes   = 250 << 20
	DocumentMaxGlobalSourceBytes = 2 << 30
	DocumentMaxTextBytes         = 1 << 20
	DocumentMaxUserTextBytes     = 25 << 20
	DocumentMaxGlobalTextBytes   = 200 << 20
	DocumentMaxChunkBytes        = 16 << 10
	DocumentMaxChunks            = 1024
	DocumentLifetime             = 30 * 24 * time.Hour
	DocumentExtractionLease      = 5 * time.Minute
	DocumentReservationLifetime  = 5 * time.Minute
)

// Document errors support errors.Is without exposing source content.
var (
	ErrDocumentQuota             = errors.New("document quota exceeded")
	ErrDocumentInvalid           = errors.New("invalid document input")
	ErrDocumentNotFound          = errors.New("document not found")
	ErrDocumentAdmissionConflict = errors.New("document admission key conflicts with existing upload")
	ErrDocumentLease             = errors.New("document extraction lease is stale or unavailable")
	ErrDocumentReservation       = errors.New("document upload reservation is expired, consumed or unavailable")
	ErrDocumentUnfenced          = errors.New("document owner is not exclusively fenced")
)

// DocumentScope selects a canonical user's library, or all libraries. Global
// scope MUST be administrator-authorized by the calling command, not this store.
type DocumentScope struct {
	UserID string
	Global bool
	// IncludeExpired exposes retained expired metadata in lists/pages only.
	IncludeExpired bool
}

// UserDocument is library metadata. IDs are opaque and never filename selectors.
// Status is queued, extracting, ready, partial, or failed.
type UserDocument struct {
	ID                                  string
	UserID, Filename, MediaType, Status string
	SourceBytes, TextBytes              int64
	AcceptedAt, ExpiresAt               time.Time
	ChunkCount                          int
}

// DocumentUpload supplies already-downloaded source bytes. A nonempty admission
// key deduplicates exact retries within this owner until deletion/expiry cleanup.
type DocumentUpload struct {
	Filename, MediaType, AdmissionKey string
	Data                              []byte
}

// DocumentChunk is one nonempty UTF-8 extraction record. Ordinals must be
// contiguous, zero-based; text is at most 16 KiB, locator 1024 bytes, method 64.
type DocumentChunk struct {
	Ordinal               int
	Text, Locator, Method string
}

// DocumentUsage includes physically retained expired rows until maintenance
// deletes them, so delayed cleanup cannot bypass disk quotas. Status counts are
// live documents only; ExpiredCount counts all expired documents regardless of
// status. Live upload reservations are separate from stored source/count totals.
// ReservedTextBytes includes BOTH extraction-job and upload reservations;
// UploadReservedTextBytes identifies the upload portion, not an additional charge.
type DocumentUsage struct {
	DocumentCount                                                                  int
	SourceBytes, TextBytes, ReservedTextBytes                                      int64
	ReservationCount, ReservedDocumentCount                                        int
	ReservedSourceBytes, UploadReservedTextBytes                                   int64
	ExpiredCount, QueuedCount, RunningCount, FailedCount, ReadyCount, PartialCount int
}

// DocumentRead pages whole chunks; NextOffset is the next zero-based ordinal.
type DocumentRead struct {
	Document   UserDocument
	Chunks     []DocumentChunk
	NextOffset int
	HasMore    bool
}

// DocumentSearchResult contains a matching whole chunk and its source metadata.
type DocumentSearchResult struct {
	Document UserDocument
	Chunk    DocumentChunk
}

// DocumentDeletion counts only committed deletions and their freed payload bytes.
type DocumentDeletion struct {
	DeletedCount           int
	SourceBytes, TextBytes int64
}

// DocumentPage is an ID-keyset page. Pass NextAfter to PageUserDocuments only
// when HasMore; concurrent mutations do not provide a frozen export snapshot.
type DocumentPage struct {
	Documents []UserDocument
	NextAfter string
	HasMore   bool
}

// DocumentExtractionJob carries the exact renewable lease and a source snapshot.
// Renewal returns a new token; never publish using the prior job after renewal.
type DocumentExtractionJob struct {
	Document                              UserDocument
	DocumentID, UserID, Owner, LeaseToken string
	LeaseUntil                            time.Time
	Attempts                              int
	Data                                  []byte
}

func documentID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func validDocumentLabel(v string, max int) bool {
	return len(v) <= max && utf8.ValidString(v) && !strings.ContainsFunc(v, unicode.IsControl)
}

func documentScopeTx(ctx context.Context, tx *sql.Tx, scope DocumentScope) error {
	if scope.Global {
		if scope.UserID != "" {
			return ErrDocumentInvalid
		}
		return nil
	}
	if strings.TrimSpace(scope.UserID) == "" {
		return ErrDocumentInvalid
	}
	return requireActiveUser(ctx, tx, scope.UserID)
}

const documentColumns = `d.id,d.canonical_user_id,d.filename,d.media_type,d.status,d.source_bytes,d.text_bytes,d.accepted_at,d.expires_at,d.chunk_count`

func scanDocument(row interface{ Scan(...any) error }) (UserDocument, error) {
	var d UserDocument
	var accepted, expires int64
	err := row.Scan(&d.ID, &d.UserID, &d.Filename, &d.MediaType, &d.Status, &d.SourceBytes, &d.TextBytes, &accepted, &expires, &d.ChunkCount)
	d.AcceptedAt, d.ExpiresAt = time.UnixMilli(accepted).UTC(), time.UnixMilli(expires).UTC()
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrDocumentNotFound
	}
	return d, err
}

func documentUsageTx(ctx context.Context, tx *sql.Tx, scope DocumentScope) (DocumentUsage, error) {
	var u DocumentUsage
	now := time.Now().UnixMilli()
	err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(source_bytes),0),coalesce(sum(text_bytes),0),coalesce(sum(reserved_bytes),0),
count(CASE WHEN expires_at<=? THEN 1 END),
count(CASE WHEN expires_at>? AND status='queued' THEN 1 END),
count(CASE WHEN expires_at>? AND status='extracting' THEN 1 END),
count(CASE WHEN expires_at>? AND status='failed' THEN 1 END),
count(CASE WHEN expires_at>? AND status='ready' THEN 1 END),
count(CASE WHEN expires_at>? AND status='partial' THEN 1 END)
FROM user_documents WHERE (? OR canonical_user_id=?)`, now, now, now, now, now, now, scope.Global, scope.UserID).Scan(&u.DocumentCount, &u.SourceBytes, &u.TextBytes, &u.ReservedTextBytes, &u.ExpiredCount, &u.QueuedCount, &u.RunningCount, &u.FailedCount, &u.ReadyCount, &u.PartialCount)
	if err != nil {
		return DocumentUsage{}, err
	}
	err = tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(file_count),0),coalesce(sum(source_bytes),0),coalesce(sum(reserved_text_bytes),0) FROM user_document_reservations WHERE expires_at>? AND (? OR canonical_user_id=?)`, now, scope.Global, scope.UserID).Scan(&u.ReservationCount, &u.ReservedDocumentCount, &u.ReservedSourceBytes, &u.UploadReservedTextBytes)
	u.ReservedTextBytes += u.UploadReservedTextBytes
	return u, err
}

func documentQuota(u DocumentUsage, global bool) error {
	if global {
		if u.SourceBytes+u.ReservedSourceBytes > DocumentMaxGlobalSourceBytes || u.TextBytes+u.ReservedTextBytes > DocumentMaxGlobalTextBytes {
			return ErrDocumentQuota
		}
	} else if u.DocumentCount+u.ReservedDocumentCount > DocumentMaxUserCount || u.SourceBytes+u.ReservedSourceBytes > DocumentMaxUserSourceBytes || u.TextBytes+u.ReservedTextBytes > DocumentMaxUserTextBytes {
		return ErrDocumentQuota
	}
	return nil
}

// documentMeasured never logs filenames, keys, queries, source bytes or extracted text.
func (s *Store) documentMeasured(ctx context.Context, operation string, started time.Time, err *error, fields ...config.Field) {
	if s.log == nil {
		return
	}
	status := "ok"
	if *err != nil {
		status = "error"
	}
	outcome := "completed"
	if errors.Is(*err, context.Canceled) {
		status, outcome = "ok", "canceled"
	} else if errors.Is(*err, ErrDocumentDiskSpace) || errors.Is(*err, ErrDocumentQuota) || errors.Is(*err, ErrDocumentInvalid) || errors.Is(*err, ErrDocumentNotFound) || errors.Is(*err, ErrDocumentAdmissionConflict) || errors.Is(*err, ErrDocumentLease) || errors.Is(*err, ErrDocumentReservation) || errors.Is(*err, ErrDocumentUnfenced) {
		status, outcome = "rejected", "rejected"
	} else if *err != nil {
		outcome = "failed"
	}
	fields = append(fields, requestctx.LogFields(ctx)...)
	fields = append(fields, config.F("is_disk_space_rejected", errors.Is(*err, ErrDocumentDiskSpace)))
	fields = append(fields, config.F("record_kind", "measurement"), config.F("operation", operation), config.F("status", status), config.F("outcome", outcome), config.F("duration_ms", time.Since(started).Milliseconds()), config.ErrorField(*err))
	if operation == "document_disk_check" || operation == "document_storage_stats" {
		s.log.Server("memory").Info("memory.documents.disk.complete", "document disk measurement completed", fields...)
	} else {
		s.log.Server("memory").Info("memory.documents.complete", "document storage operation completed", fields...)
	}
}

// AcceptUserDocuments atomically admits metadata, BLOBs, reservations, and jobs.
// Missing/inactive canonical owners are rejected; authentication/ban admission
// belongs to the caller. The SQLite immediate transaction serializes all quotas,
// which are checked before any source BLOB insertion, including live reservations.
func (s *Store) AcceptUserDocuments(ctx context.Context, userID string, uploads []DocumentUpload) ([]UserDocument, error) {
	return s.acceptUserDocuments(ctx, userID, "", uploads)
}

func (s *Store) acceptUserDocuments(ctx context.Context, userID, reservationID string, uploads []DocumentUpload) (docs []UserDocument, err error) {
	started := time.Now()
	admitted, replayed := 0, 0
	defer func() {
		if err != nil {
			admitted, replayed = 0, 0
		}
		s.documentMeasured(ctx, "document_accept", started, &err, config.F("accepted_count", admitted), config.F("replayed_count", replayed))
	}()
	if len(uploads) == 0 || len(uploads) > DocumentMaxRequestFiles {
		return nil, ErrDocumentInvalid
	}
	total := 0
	for _, u := range uploads {
		if strings.TrimSpace(u.Filename) == "" || !validDocumentLabel(u.Filename, 255) || strings.ContainsAny(u.Filename, `/\`) || u.Filename == "." || u.Filename == ".." || strings.TrimSpace(u.MediaType) == "" || !validDocumentLabel(u.MediaType, 255) || !validDocumentLabel(u.AdmissionKey, 256) {
			return nil, ErrDocumentInvalid
		}
		if len(u.Data) == 0 || len(u.Data) > DocumentMaxSourceBytes {
			return nil, ErrDocumentQuota
		}
		total += len(u.Data)
	}
	if total > DocumentMaxRequestBytes {
		return nil, ErrDocumentQuota
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = documentScopeTx(ctx, tx, DocumentScope{UserID: userID}); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	var reservationExpires int64
	if reservationID != "" {
		var count int
		var sourceBytes int64
		err = tx.QueryRowContext(ctx, `SELECT file_count,source_bytes,expires_at FROM user_document_reservations WHERE id=? AND canonical_user_id=? AND expires_at>?`, reservationID, userID, now.UnixMilli()).Scan(&count, &sourceBytes, &reservationExpires)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrDocumentReservation
		}
		if err != nil {
			return nil, err
		}
		if len(uploads) > count || int64(total) > sourceBytes {
			return nil, ErrDocumentQuota
		}
		// Consumption and document admission commit together; this removes only our
		// own reserved capacity before the actual document charges are checked.
		if _, err = tx.ExecContext(ctx, `DELETE FROM user_document_reservations WHERE id=? AND canonical_user_id=?`, reservationID, userID); err != nil {
			return nil, err
		}
	}
	var sources []struct {
		id   string
		data []byte
	}
	for _, u := range uploads {
		hash := sha256.Sum256(u.Data)
		digest := hex.EncodeToString(hash[:])
		if u.AdmissionKey != "" {
			var id, oldHash, filename, media string
			e := tx.QueryRowContext(ctx, `SELECT id,source_hash,filename,media_type FROM user_documents WHERE canonical_user_id=? AND admission_key=?`, userID, u.AdmissionKey).Scan(&id, &oldHash, &filename, &media)
			if e == nil {
				if digest != oldHash || filename != u.Filename || media != u.MediaType {
					return nil, ErrDocumentAdmissionConflict
				}
				d, e := scanDocument(tx.QueryRowContext(ctx, `SELECT `+documentColumns+` FROM user_documents d WHERE d.id=? AND d.expires_at>?`, id, now.UnixMilli()))
				if e != nil {
					return nil, e
				}
				docs = append(docs, d)
				replayed++
				continue
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return nil, e
			}
		}
		d := UserDocument{ID: documentID(), UserID: userID, Filename: u.Filename, MediaType: u.MediaType, Status: "queued", SourceBytes: int64(len(u.Data)), AcceptedAt: now, ExpiresAt: now.Add(DocumentLifetime)}
		var key any
		if u.AdmissionKey != "" {
			key = u.AdmissionKey
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO user_documents(id,canonical_user_id,filename,media_type,admission_key,source_hash,status,source_bytes,accepted_at,expires_at) VALUES(?,?,?,?,?,?,'queued',?,?,?)`, d.ID, userID, u.Filename, u.MediaType, key, digest, d.SourceBytes, now.UnixMilli(), d.ExpiresAt.UnixMilli()); err != nil {
			return nil, err
		}
		sources = append(sources, struct {
			id   string
			data []byte
		}{d.ID, u.Data})
		if _, err = tx.ExecContext(ctx, `INSERT INTO user_document_extraction_jobs(document_id,state) VALUES(?,'queued')`, d.ID); err != nil {
			return nil, err
		}
		docs = append(docs, d)
		admitted++
	}
	for _, scope := range []DocumentScope{{UserID: userID}, {Global: true}} {
		usage, e := documentUsageTx(ctx, tx, scope)
		if e != nil {
			return nil, e
		}
		if e = documentQuota(usage, scope.Global); e != nil {
			return nil, e
		}
		if scope.Global && len(sources) > 0 {
			var sourceBytes int64
			for _, source := range sources {
				sourceBytes += int64(len(source.data))
			}
			if e = s.checkDocumentDiskTx(ctx, tx, usage, sourceBytes); e != nil {
				return nil, e
			}
		}
	}
	for _, source := range sources {
		if _, err = tx.ExecContext(ctx, `INSERT INTO user_document_sources(document_id,data) VALUES(?,?)`, source.id, source.data); err != nil {
			return nil, err
		}
	}
	if reservationID != "" && time.Now().UnixMilli() >= reservationExpires {
		return nil, ErrDocumentReservation
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return docs, nil
}

// ListUserDocuments returns at most 50 documents, ordered by opaque ID, excluding
// expired metadata unless scope.IncludeExpired is set.
// Administrators should use PageUserDocuments to enumerate a larger library.
func (s *Store) ListUserDocuments(ctx context.Context, scope DocumentScope) ([]UserDocument, error) {
	page, err := s.PageUserDocuments(ctx, scope, "", 50)
	return page.Documents, err
}

// PageUserDocuments returns at most 50 metadata records after an exact ID.
// Expired records are omitted unless scope.IncludeExpired is set.
func (s *Store) PageUserDocuments(ctx context.Context, scope DocumentScope, after string, limit int) (page DocumentPage, err error) {
	defer s.documentMeasured(ctx, "document_list", time.Now(), &err)
	if limit <= 0 {
		limit = 50
	}
	if limit > 50 {
		limit = 50
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	if err = documentScopeTx(ctx, tx, scope); err != nil {
		return page, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+documentColumns+` FROM user_documents d WHERE (? OR d.canonical_user_id=?) AND (? OR d.expires_at>?) AND d.id>? ORDER BY d.id LIMIT ?`, scope.Global, scope.UserID, scope.IncludeExpired, time.Now().UnixMilli(), after, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		d, e := scanDocument(rows)
		if e != nil {
			return DocumentPage{}, e
		}
		page.Documents = append(page.Documents, d)
	}
	if err = rows.Err(); err != nil {
		return DocumentPage{}, err
	}
	if len(page.Documents) > limit {
		page.HasMore = true
		page.Documents = page.Documents[:limit]
	}
	if len(page.Documents) > 0 {
		page.NextAfter = page.Documents[len(page.Documents)-1].ID
	}
	return page, nil
}

// UserDocumentUsage reports retained physical quota consumption, including leases.
func (s *Store) UserDocumentUsage(ctx context.Context, scope DocumentScope) (usage DocumentUsage, err error) {
	defer s.documentMeasured(ctx, "document_usage", time.Now(), &err)
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return usage, err
	}
	defer tx.Rollback()
	if err = documentScopeTx(ctx, tx, scope); err != nil {
		return usage, err
	}
	return documentUsageTx(ctx, tx, scope)
}

// ReadUserDocument returns whole chunks starting at offset (not a byte offset).
// Limit defaults to four and is capped at eight (at most 128 KiB of text).
func (s *Store) ReadUserDocument(ctx context.Context, userID, id string, offset, limit int) (out DocumentRead, err error) {
	defer s.documentMeasured(ctx, "document_read", time.Now(), &err)
	if offset < 0 {
		return out, ErrDocumentInvalid
	}
	if limit <= 0 {
		limit = 4
	}
	if limit > 8 {
		limit = 8
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err = documentScopeTx(ctx, tx, DocumentScope{UserID: userID}); err != nil {
		return out, err
	}
	out.Document, err = scanDocument(tx.QueryRowContext(ctx, `SELECT `+documentColumns+` FROM user_documents d WHERE d.id=? AND d.canonical_user_id=? AND d.expires_at>?`, id, userID, time.Now().UnixMilli()))
	if err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT ordinal,text,locator,method FROM user_document_chunks WHERE document_id=? AND ordinal>=? ORDER BY ordinal LIMIT ?`, id, offset, limit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.NextOffset = offset
	for rows.Next() {
		var c DocumentChunk
		if err = rows.Scan(&c.Ordinal, &c.Text, &c.Locator, &c.Method); err != nil {
			return DocumentRead{}, err
		}
		out.Chunks = append(out.Chunks, c)
		out.NextOffset = c.Ordinal + 1
	}
	if err = rows.Err(); err != nil {
		return DocumentRead{}, err
	}
	out.HasMore = out.NextOffset < out.Document.ChunkCount
	return out, nil
}

// searchUserDocumentsLexical ranks canonical matches across the quota-bounded
// private library and returns match-centered excerpts of at most 1500 runes.
// Empty documentID searches all live ready/partial documents. No semantic search,
// FTS, or stemming is implied; ties use document ID then chunk ordinal.
func (s *Store) searchUserDocumentsLexical(ctx context.Context, userID, query, documentID string, limit int) (out []DocumentSearchResult, err error) {
	query = strings.TrimSpace(query)
	if query == "" || !utf8.ValidString(query) || len(query) > 1024 || utf8.RuneCountInString(query) > 400 {
		return nil, ErrDocumentInvalid
	}
	if limit <= 0 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = documentScopeTx(ctx, tx, DocumentScope{UserID: userID}); err != nil {
		return nil, err
	}
	terms := documentSearchTerms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	predicate, patterns := documentSearchPredicate(terms)
	args := []any{userID, time.Now().UnixMilli(), documentID, documentID}
	args = append(args, patterns...)
	rows, err := tx.QueryContext(ctx, `SELECT c.document_id,c.ordinal,c.text,c.locator,c.method FROM user_document_chunks c JOIN user_documents d ON d.id=c.document_id WHERE d.canonical_user_id=? AND d.expires_at>? AND (?='' OR d.id=?) AND d.status IN ('ready','partial') AND (`+predicate+`) ORDER BY d.id,c.ordinal`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var scores []int
	scannedBytes := 0
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		var r DocumentSearchResult
		if err = rows.Scan(&r.Document.ID, &r.Chunk.Ordinal, &r.Chunk.Text, &r.Chunk.Locator, &r.Chunk.Method); err != nil {
			rows.Close()
			return nil, err
		}
		scannedBytes += len(r.Chunk.Text)
		if scannedBytes > DocumentMaxUserTextBytes {
			return nil, ErrDocumentQuota
		}
		score, excerpt := documentSearchMatch(r.Chunk.Text, terms)
		if score == 0 {
			continue
		}
		at := 0
		for at < len(scores) && scores[at] >= score {
			at++
		}
		if at < limit {
			r.Chunk.Text = excerpt
			out = append(out, DocumentSearchResult{})
			copy(out[at+1:], out[at:])
			out[at] = r
			scores = append(scores, 0)
			copy(scores[at+1:], scores[at:])
			scores[at] = score
			if len(out) > limit {
				out, scores = out[:limit], scores[:limit]
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Document, err = scanDocument(tx.QueryRowContext(ctx, `SELECT `+documentColumns+` FROM user_documents d WHERE d.id=? AND d.canonical_user_id=?`, out[i].Document.ID, userID))
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteUserDocuments deletes up to 50 distinct exact IDs atomically. Unknown or
// other-owner IDs fail the entire operation; expired owned IDs remain deletable.
func (s *Store) DeleteUserDocuments(ctx context.Context, scope DocumentScope, ids []string) (out DocumentDeletion, err error) {
	return s.deleteUserDocuments(ctx, scope, ids, nil)
}

// DeleteUserDocumentsFenced also requires every selected row's current canonical
// owner to be in heldOwners, checked in the deletion transaction. Empty owners
// grant no deletion. Callers must supply actually held exclusive fences and must
// separately authorize scope; a fence grants no administrator permission.
func (s *Store) DeleteUserDocumentsFenced(ctx context.Context, scope DocumentScope, ids, heldOwners []string) (DocumentDeletion, error) {
	owners := make(map[string]bool, len(heldOwners))
	for _, owner := range heldOwners {
		owners[owner] = true
	}
	return s.deleteUserDocuments(ctx, scope, ids, owners)
}

func (s *Store) deleteUserDocuments(ctx context.Context, scope DocumentScope, ids []string, heldOwners map[string]bool) (out DocumentDeletion, err error) {
	started := time.Now()
	defer func() {
		s.documentMeasured(ctx, "document_delete", started, &err, config.F("deleted_count", out.DeletedCount), config.F("source_bytes", out.SourceBytes), config.F("text_bytes", out.TextBytes))
	}()
	if len(ids) == 0 || len(ids) > 50 {
		return out, ErrDocumentInvalid
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err = documentScopeTx(ctx, tx, scope); err != nil {
		return out, err
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return DocumentDeletion{}, ErrDocumentInvalid
		}
		seen[id] = true
		d, e := scanDocument(tx.QueryRowContext(ctx, `SELECT `+documentColumns+` FROM user_documents d WHERE d.id=? AND (? OR d.canonical_user_id=?)`, id, scope.Global, scope.UserID))
		if e != nil {
			return DocumentDeletion{}, e
		}
		if heldOwners != nil && !heldOwners[d.UserID] {
			return DocumentDeletion{}, ErrDocumentUnfenced
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM user_documents WHERE id=?`, id); err != nil {
			return DocumentDeletion{}, err
		}
		out.DeletedCount++
		out.SourceBytes += d.SourceBytes
		out.TextBytes += d.TextBytes
	}
	if err = tx.Commit(); err != nil {
		return DocumentDeletion{}, err
	}
	s.signalDerivedIndex()
	return out, nil
}

// mergeUserDocumentsTx preserves all sources or rejects the entire account merge
// on quota overflow. Admission keys survive unless they collide with the winner's
// namespace; in that case the winner's replay mapping takes precedence.
func mergeUserDocumentsTx(ctx context.Context, tx *sql.Tx, winner, loser string) error {
	// In-flight downloads were admitted for the old owner and cannot transfer.
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_document_reservations WHERE canonical_user_id=?`, loser); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE user_document_extraction_jobs SET state='queued',lease_owner='',lease_token='',lease_until=0 WHERE state='running' AND document_id IN (SELECT id FROM user_documents WHERE canonical_user_id=?)`, loser); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE user_documents AS loser SET admission_key=NULL WHERE canonical_user_id=? AND EXISTS(SELECT 1 FROM user_documents winner WHERE winner.canonical_user_id=? AND winner.admission_key=loser.admission_key)`, loser, winner); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE user_documents SET canonical_user_id=?,status=CASE WHEN status='extracting' THEN 'queued' ELSE status END WHERE canonical_user_id=?`, winner, loser); err != nil {
		return err
	}
	u, err := documentUsageTx(ctx, tx, DocumentScope{UserID: winner})
	if err != nil {
		return err
	}
	if err = documentQuota(u, false); err != nil {
		return fmt.Errorf("merge document library: %w", err)
	}
	return nil
}
