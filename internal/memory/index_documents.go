package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// UserDocumentIndexRecord is a tenant-owned, currently eligible chunk. ID is an
// internal non-reusable surrogate, never a model-visible document selector.
type UserDocumentIndexRecord struct {
	ID           int64
	UserID, Text string
}

const documentIndexFrom = ` FROM user_document_chunk_ids key JOIN user_document_chunks c ON c.document_id=key.document_id AND c.ordinal=key.ordinal JOIN user_documents d ON d.id=c.document_id `
const documentIndexEligible = `d.status IN ('ready','partial') AND d.expires_at>?`

// UserDocumentIndexRecords pages eligible canonical chunks by exclusive ID.
func (s *Store) UserDocumentIndexRecords(ctx context.Context, after int64, limit int) ([]UserDocumentIndexRecord, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.sql.QueryContext(ctx, `SELECT key.id,d.canonical_user_id,c.text`+documentIndexFrom+`WHERE `+documentIndexEligible+` AND key.id>? ORDER BY key.id LIMIT ?`, time.Now().UnixMilli(), after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []UserDocumentIndexRecord
	for rows.Next() {
		var r UserDocumentIndexRecord
		if err := rows.Scan(&r.ID, &r.UserID, &r.Text); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// UserDocumentIndexRecordByID rechecks current eligibility and ownership.
func (s *Store) UserDocumentIndexRecordByID(ctx context.Context, id int64, userID string) (UserDocumentIndexRecord, error) {
	var r UserDocumentIndexRecord
	err := s.sql.QueryRowContext(ctx, `SELECT key.id,d.canonical_user_id,c.text`+documentIndexFrom+`WHERE `+documentIndexEligible+` AND key.id=? AND d.canonical_user_id=?`, time.Now().UnixMilli(), id, userID).Scan(&r.ID, &r.UserID, &r.Text)
	return r, err
}

// WriteUserDocumentIndexRecord fences publication against deletion, expiry,
// ownership moves and changed text in the same write transaction.
func (s *Store) WriteUserDocumentIndexRecord(ctx context.Context, revision DerivedIndexRevision, record UserDocumentIndexRecord, vector []float64) error {
	if err := validateRevisionTableIdentity(revision); err != nil {
		return err
	}
	if revision.Kind != IndexKindUserDocumentFTS && (revision.Kind != IndexKindUserDocumentVector || len(vector) != revision.Dimension) {
		return fmt.Errorf("invalid document index write")
	}
	if s.indexWriteHook != nil {
		s.indexWriteHook("before_recheck")
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current UserDocumentIndexRecord
	err = tx.QueryRowContext(ctx, `SELECT key.id,d.canonical_user_id,c.text`+documentIndexFrom+`WHERE `+documentIndexEligible+` AND key.id=? AND d.canonical_user_id=?`, time.Now().UnixMilli(), record.ID, record.UserID).Scan(&current.ID, &current.UserID, &current.Text)
	stale := errors.Is(err, sql.ErrNoRows) || (err == nil && current != record)
	if err != nil && !stale {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM `+revision.TableName+` WHERE rowid=? AND canonical_user_id=?`, record.ID, record.UserID); err != nil {
		return err
	}
	if stale {
		if err := tx.Commit(); err != nil {
			return err
		}
		return ErrStaleIndexRecord
	}
	// A live canonical recheck authorizes replacing an old owner's projection
	// after an account merge. Stale callers above can delete only their own row.
	if _, err = tx.ExecContext(ctx, `DELETE FROM `+revision.TableName+` WHERE rowid=?`, record.ID); err != nil {
		return err
	}
	if s.indexWriteHook != nil {
		s.indexWriteHook("after_recheck")
	}
	if revision.Kind == IndexKindUserDocumentFTS {
		_, err = tx.ExecContext(ctx, `INSERT INTO `+revision.TableName+`(rowid,canonical_user_id,text) VALUES (?,?,?)`, record.ID, record.UserID, record.Text)
	} else {
		var data []byte
		data, err = serializeVector(vector)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO `+revision.TableName+`(rowid,canonical_user_id,embedding_model,canonical_version,embedding) VALUES (?,?,?,?,?)`, record.ID, record.UserID, revision.Model, record.Text, data)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func documentIndexValidationSQL(revision DerivedIndexRevision) (string, string) {
	expected := `SELECT COUNT(*)` + documentIndexFrom + `WHERE ` + documentIndexEligible
	valid := `SELECT COUNT(*)` + documentIndexFrom + `JOIN ` + revision.TableName + ` idx ON idx.rowid=key.id AND idx.canonical_user_id=d.canonical_user_id WHERE ` + documentIndexEligible
	if revision.Kind == IndexKindUserDocumentVector {
		valid += ` AND idx.embedding_model=? AND idx.canonical_version=c.text`
	} else {
		valid += ` AND idx.text=c.text`
	}
	return expected, valid
}
