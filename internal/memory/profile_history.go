package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// RecentCompletedExchangesAfter returns newest complete delivered exchanges.
func (s *ProfileStore) RecentCompletedExchangesAfter(ctx context.Context, owner, key string, generation int, after int64, limit int) ([]SessionTurn, error) {
	return s.deliveredProfileTurns(ctx, owner, key, generation, after, limit, true)
}

type profileSummary struct {
	Version int            `json:"version"`
	Summary SessionSummary `json:"summary"`
}

func profileSummaryKey(id string) string { return "oswald:v1:summary:" + id }

// LatestSessionSummary reads the current generation's durable checkpoint.
func (s *ProfileStore) LatestSessionSummary(ctx context.Context, owner, key string, generation int) (SessionSummary, error) {
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return SessionSummary{}, err
	}
	defer tx.Rollback()
	id, err := s.activeSession(ctx, tx, owner, key, generation)
	if err != nil {
		return SessionSummary{}, err
	}
	var encoded string
	err = tx.QueryRowContext(ctx, `SELECT value FROM state_meta WHERE key=?`, profileSummaryKey(id)).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionSummary{}, nil
	}
	if err != nil {
		return SessionSummary{}, err
	}
	var stored profileSummary
	if len(encoded) > 64*1024 || json.Unmarshal([]byte(encoded), &stored) != nil || stored.Version != 1 || stored.Summary.UserID != owner || stored.Summary.SessionID != key || stored.Summary.SessionGeneration != generation {
		return SessionSummary{}, errors.New("invalid profile summary")
	}
	return stored.Summary, tx.Commit()
}
