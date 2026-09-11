package memory

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// ClaimUserDocumentExtraction claims the oldest queued or abandoned job and
// loads its source BLOB in the same transaction. No eligible work returns nil,nil.
// Crashed workers can be reclaimed after five minutes, including after restart.
func (s *Store) ClaimUserDocumentExtraction(ctx context.Context, owner string) (job *DocumentExtractionJob, err error) {
	started := time.Now()
	defer func() { s.documentMeasured(ctx, "document_claim", started, &err, config.F("is_claimed", job != nil)) }()
	if strings.TrimSpace(owner) == "" || !validDocumentLabel(owner, 256) {
		return nil, ErrDocumentInvalid
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Truncate(time.Millisecond)
	var id string
	err = tx.QueryRowContext(ctx, `SELECT d.id FROM user_documents d JOIN user_document_extraction_jobs j ON j.document_id=d.id JOIN account_users u ON u.canonical_user_id=d.canonical_user_id WHERE u.lifecycle_state='active' AND u.is_banned=0 AND d.expires_at>? AND (j.state='queued' OR (j.state='running' AND j.lease_until<=?)) ORDER BY d.accepted_at,d.id LIMIT 1`, now.UnixMilli(), now.UnixMilli()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	job = &DocumentExtractionJob{DocumentID: id, Owner: owner, LeaseToken: documentID(), LeaseUntil: now.Add(DocumentExtractionLease)}
	if _, err = tx.ExecContext(ctx, `UPDATE user_document_extraction_jobs SET state='running',lease_owner=?,lease_token=?,lease_until=?,attempts=attempts+1 WHERE document_id=?`, owner, job.LeaseToken, job.LeaseUntil.UnixMilli(), id); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user_documents SET status='extracting' WHERE id=?`, id); err != nil {
		return nil, err
	}
	job.Document, err = scanDocument(tx.QueryRowContext(ctx, `SELECT `+documentColumns+` FROM user_documents d WHERE d.id=?`, id))
	if err != nil {
		return nil, err
	}
	job.UserID = job.Document.UserID
	if err = tx.QueryRowContext(ctx, `SELECT s.data,j.attempts FROM user_document_sources s JOIN user_document_extraction_jobs j ON j.document_id=s.document_id WHERE s.document_id=?`, id).Scan(&job.Data, &job.Attempts); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return job, nil
}

func documentLeaseTx(ctx context.Context, tx *sql.Tx, job *DocumentExtractionJob, now time.Time) error {
	if job == nil || job.DocumentID == "" || job.UserID == "" || job.Owner == "" || job.LeaseToken == "" {
		return ErrDocumentLease
	}
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM user_document_extraction_jobs j JOIN user_documents d ON d.id=j.document_id JOIN account_users u ON u.canonical_user_id=d.canonical_user_id WHERE j.document_id=? AND d.canonical_user_id=? AND u.lifecycle_state='active' AND u.is_banned=0 AND d.expires_at>? AND d.status='extracting' AND j.state='running' AND j.lease_owner=? AND j.lease_token=? AND j.lease_until=? AND j.lease_until>?`, job.DocumentID, job.UserID, now.UnixMilli(), job.Owner, job.LeaseToken, job.LeaseUntil.UnixMilli(), now.UnixMilli()).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDocumentLease
	}
	return err
}

// RenewUserDocumentExtraction rotates the exact lease token while preserving
// the source snapshot. Expired, merged, deleted, banned or replaced jobs fail.
func (s *Store) RenewUserDocumentExtraction(ctx context.Context, job *DocumentExtractionJob) (renewed *DocumentExtractionJob, err error) {
	defer s.documentMeasured(ctx, "document_renew", time.Now(), &err)
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err = documentLeaseTx(ctx, tx, job, now); err != nil {
		return nil, err
	}
	copy := *job
	copy.LeaseToken = documentID()
	copy.LeaseUntil = now.Add(DocumentExtractionLease)
	if _, err = tx.ExecContext(ctx, `UPDATE user_document_extraction_jobs SET lease_token=?,lease_until=? WHERE document_id=?`, copy.LeaseToken, copy.LeaseUntil.UnixMilli(), copy.DocumentID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &copy, nil
}

// CompleteUserDocumentExtraction atomically publishes validated chunks, releases
// the reservation, and terminates the exact live job. Empty results are permitted;
// partial marks a successfully extracted but incomplete document. TextBytes counts
// text, locator and method UTF-8 bytes, preventing metadata from bypassing quotas.
func (s *Store) CompleteUserDocumentExtraction(ctx context.Context, job *DocumentExtractionJob, chunks []DocumentChunk, partial bool) (err error) {
	started := time.Now()
	defer func() {
		count := 0
		if err == nil {
			count = len(chunks)
		}
		s.documentMeasured(ctx, "document_complete", started, &err, config.F("chunk_count", count), config.F("is_partial", partial))
	}()
	if len(chunks) > DocumentMaxChunks {
		return ErrDocumentQuota
	}
	var textBytes int64
	for i, c := range chunks {
		if c.Ordinal != i || strings.TrimSpace(c.Text) == "" || !utf8.ValidString(c.Text) || len(c.Text) > DocumentMaxChunkBytes || !validDocumentLabel(c.Locator, 1024) || !validDocumentLabel(c.Method, 64) {
			return ErrDocumentInvalid
		}
		textBytes += int64(len(c.Text) + len(c.Locator) + len(c.Method))
	}
	if textBytes > DocumentMaxTextBytes {
		return ErrDocumentQuota
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = documentLeaseTx(ctx, tx, job, time.Now()); err != nil {
		return err
	}
	for _, c := range chunks {
		if _, err = tx.ExecContext(ctx, `INSERT INTO user_document_chunks(document_id,ordinal,text,locator,method) VALUES(?,?,?,?,?)`, job.DocumentID, c.Ordinal, c.Text, c.Locator, c.Method); err != nil {
			return err
		}
	}
	// Revalidate using persisted ownership and expiry, not caller-supplied metadata.
	if err = documentLeaseTx(ctx, tx, job, time.Now()); err != nil {
		return err
	}
	status := "ready"
	if partial {
		status = "partial"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user_documents SET status=?,text_bytes=?,reserved_bytes=0,chunk_count=? WHERE id=?`, status, textBytes, len(chunks), job.DocumentID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user_document_extraction_jobs SET state='succeeded',lease_owner='',lease_token='',lease_until=0 WHERE document_id=?`, job.DocumentID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.signalDerivedIndex()
	return nil
}

// FailUserDocumentExtraction terminally fails an exact live lease and releases
// its text reservation. Code must be a bounded machine label, never raw errors.
// Source bytes remain stored until deletion or the 30-day expiry cleanup.
func (s *Store) FailUserDocumentExtraction(ctx context.Context, job *DocumentExtractionJob, code string) (err error) {
	defer s.documentMeasured(ctx, "document_fail", time.Now(), &err)
	if len(code) == 0 || len(code) > 64 || strings.ContainsFunc(code, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') }) {
		return ErrDocumentInvalid
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = documentLeaseTx(ctx, tx, job, time.Now()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user_document_extraction_jobs SET state='failed',failure_code=?,lease_owner='',lease_token='',lease_until=0 WHERE document_id=?`, code, job.DocumentID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user_documents SET status='failed',reserved_bytes=0 WHERE id=?`, job.DocumentID); err != nil {
		return err
	}
	return tx.Commit()
}
