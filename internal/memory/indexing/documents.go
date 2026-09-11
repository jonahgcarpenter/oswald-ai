package indexing

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

func (s *Service) buildUserDocuments(ctx context.Context, revision memory.DerivedIndexRevision) error {
	var after int64
	for {
		records, err := s.store.UserDocumentIndexRecords(ctx, after, batchSize)
		if err != nil {
			return err
		}
		for _, record := range records {
			if err := s.writeCurrentUserDocument(ctx, revision, record); err != nil {
				return err
			}
			after = record.ID
		}
		if len(records) < batchSize {
			return nil
		}
	}
}

func (s *Service) writeCurrentUserDocument(ctx context.Context, revision memory.DerivedIndexRevision, record memory.UserDocumentIndexRecord) error {
	for attempt := 0; attempt < 3; attempt++ {
		var vector []float64
		if revision.Kind == memory.IndexKindUserDocumentVector {
			var err error
			vector, err = s.embed(ctx, revision.Model, record.Text)
			if err != nil {
				return err
			}
		}
		err := s.store.WriteUserDocumentIndexRecord(ctx, revision, record, vector)
		if !errors.Is(err, memory.ErrStaleIndexRecord) {
			return err
		}
		record, err = s.store.UserDocumentIndexRecordByID(ctx, record.ID, record.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return memory.ErrStaleIndexRecord
}
