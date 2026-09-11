package memory

import (
	"context"
	"errors"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// DocumentReservation is single-use download capacity for one canonical owner.
// SourceBytes is the aggregate download ceiling, not an estimate allowed to grow.
// It expires after five minutes without renewal. The gateway must enforce both
// this aggregate ceiling and the per-file 20 MiB limit while downloading.
type DocumentReservation struct {
	ID, UserID                     string
	FileCount                      int
	SourceBytes, ReservedTextBytes int64
	CreatedAt, ExpiresAt           time.Time
}

// ReserveUserDocumentUpload reserves file slots, source bytes, and 1 MiB of
// extracted capacity per file before any download. Live reservations participate
// in every admission/merge quota check across all store handles.
func (s *Store) ReserveUserDocumentUpload(ctx context.Context, userID string, fileCount int, sourceBytes int64) (reservation DocumentReservation, err error) {
	started := time.Now()
	defer func() {
		s.documentMeasured(ctx, "document_reserve", started, &err, config.F("reserved_count", reservation.FileCount), config.F("source_bytes", reservation.SourceBytes))
	}()
	if fileCount < 1 || fileCount > DocumentMaxRequestFiles || sourceBytes < int64(fileCount) {
		return DocumentReservation{}, ErrDocumentInvalid
	}
	if sourceBytes > DocumentMaxRequestBytes || sourceBytes > int64(fileCount)*DocumentMaxSourceBytes {
		return DocumentReservation{}, ErrDocumentQuota
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return DocumentReservation{}, err
	}
	defer tx.Rollback()
	if err = documentScopeTx(ctx, tx, DocumentScope{UserID: userID}); err != nil {
		return DocumentReservation{}, err
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	// Bound expired metadata cleanup per admission, as well as during maintenance.
	if _, err = tx.ExecContext(ctx, `DELETE FROM user_document_reservations WHERE id IN (SELECT id FROM user_document_reservations WHERE expires_at<=? ORDER BY expires_at,id LIMIT 100)`, now.UnixMilli()); err != nil {
		return DocumentReservation{}, err
	}
	reservedText := int64(fileCount) * DocumentMaxTextBytes
	for _, scope := range []DocumentScope{{UserID: userID}, {Global: true}} {
		u, e := documentUsageTx(ctx, tx, scope)
		if e != nil {
			return DocumentReservation{}, e
		}
		u.ReservedDocumentCount += fileCount
		u.ReservedSourceBytes += sourceBytes
		u.ReservedTextBytes += reservedText
		if e = documentQuota(u, scope.Global); e != nil {
			return DocumentReservation{}, e
		}
		if scope.Global {
			if e = s.checkDocumentDiskTx(ctx, tx, u, 0); e != nil {
				return DocumentReservation{}, e
			}
		}
	}
	r := DocumentReservation{ID: documentID(), UserID: userID, FileCount: fileCount, SourceBytes: sourceBytes, ReservedTextBytes: reservedText, CreatedAt: now, ExpiresAt: now.Add(DocumentReservationLifetime)}
	if _, err = tx.ExecContext(ctx, `INSERT INTO user_document_reservations(id,canonical_user_id,file_count,source_bytes,reserved_text_bytes,created_at,expires_at) VALUES(?,?,?,?,?,?,?)`, r.ID, r.UserID, r.FileCount, r.SourceBytes, r.ReservedTextBytes, r.CreatedAt.UnixMilli(), r.ExpiresAt.UnixMilli()); err != nil {
		return DocumentReservation{}, err
	}
	if err = tx.Commit(); err != nil {
		return DocumentReservation{}, err
	}
	return r, nil
}

// ReleaseUserDocumentUpload idempotently releases only the named owner's exact
// reservation. Missing, consumed or other-owner IDs are no-ops, not disclosures.
// Callers must release reservations when downloads fail or are abandoned.
func (s *Store) ReleaseUserDocumentUpload(ctx context.Context, userID, reservationID string) (err error) {
	started := time.Now()
	var released int64
	defer func() {
		s.documentMeasured(ctx, "document_release", started, &err, config.F("released_count", released))
	}()
	if userID == "" || reservationID == "" {
		return ErrDocumentInvalid
	}
	result, err := s.sql.ExecContext(ctx, `DELETE FROM user_document_reservations WHERE canonical_user_id=? AND id=?`, userID, reservationID)
	if err == nil {
		released, err = result.RowsAffected()
	}
	return err
}

// AcceptReservedUserDocuments consumes a live reservation atomically with document
// admission. Fewer files/bytes are allowed; unused capacity is released. A consumed
// reservation cannot be replayed. AdmissionKey still deduplicates retries admitted
// under a NEW reservation. Every failed call attempts bounded independent release
// after rollback, including caller cancellation; failed cleanup leaves only a
// five-minute reservation and is included in the returned error.
func (s *Store) AcceptReservedUserDocuments(ctx context.Context, userID, reservationID string, uploads []DocumentUpload) ([]UserDocument, error) {
	if reservationID == "" {
		err := ErrDocumentInvalid
		s.documentMeasured(ctx, "document_accept", time.Now(), &err, config.F("accepted_count", 0), config.F("replayed_count", 0))
		return nil, err
	}
	docs, err := s.acceptUserDocuments(ctx, userID, reservationID, uploads)
	if err != nil {
		// Download/request cancellation must not prevent reclaiming unused capacity.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = errors.Join(err, s.ReleaseUserDocumentUpload(cleanup, userID, reservationID))
	}
	return docs, err
}
