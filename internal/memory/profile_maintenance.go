package memory

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// SweepProfile expires inactive conversations and timed-out deliveries in
// bounded transactions. Generation high-water rows and operator files remain.
func (s *ProfileStore) SweepProfile(ctx context.Context) (resultErr error) {
	started := time.Now()
	messageCount, sessionCount, failedCount := int64(0), int64(0), int64(0)
	defer func() {
		if s.log != nil {
			status := "ok"
			if resultErr != nil {
				status = "error"
			}
			s.log.Server("maintenance").Info("maintenance.profile.complete", "completed profile maintenance", config.F("record_kind", "measurement"), config.F("user_id", s.profile), config.F("status", status), config.F("message_deleted_count", messageCount), config.F("session_expired_count", sessionCount), config.F("delivery_failed_count", failedCount), config.F("duration_ms", time.Since(started).Milliseconds()), config.ErrorField(resultErr))
		}
	}()
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := float64(s.now().UnixNano()) / 1e9
	rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions WHERE profile_name=? AND expiry_finalized=0 AND (ended_at IS NOT NULL OR last_activity_at+json_extract(origin_json,'$.ttl_seconds')<=?) ORDER BY started_at LIMIT 100`, s.profile, now)
	if err != nil {
		return err
	}
	var expired []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, id)
	}
	iterationErr := rows.Err()
	rows.Close()
	if iterationErr != nil {
		return iterationErr
	}
	var deleted int64
	for _, id := range expired {
		result, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE session_id=?`, id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		deleted += count
		for _, prefix := range []string{"oswald:v1:turn:", "oswald:v1:compression:", "oswald:v1:campaign:"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM state_meta WHERE substr(key,1,?)=?`, len(prefix+id+":"), prefix+id+":"); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM state_meta WHERE key IN (?,?)`, snapshotKey(id), profileSummaryKey(id)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM compression_locks WHERE session_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET ended_at=COALESCE(ended_at,?),end_reason=COALESCE(end_reason,'expired'),expiry_finalized=1 WHERE id=?`, now, id); err != nil {
			return err
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT v.key,v.value FROM messages m JOIN state_meta v ON v.key=('oswald:v1:turn:'||m.session_id||':'||m.id) JOIN sessions s ON s.id=m.session_id WHERE s.profile_name=? AND m.timestamp<=? AND json_extract(v.value,'$.delivery')='pending' ORDER BY m.id LIMIT 100`, s.profile, now-900)
	if err != nil {
		return err
	}
	type failure struct{ key, value string }
	var failures []failure
	for rows.Next() {
		var item failure
		if err := rows.Scan(&item.key, &item.value); err != nil {
			rows.Close()
			return err
		}
		failures = append(failures, item)
	}
	iterationErr = rows.Err()
	rows.Close()
	if iterationErr != nil {
		return iterationErr
	}
	for _, item := range failures {
		state, err := decodeProfileExchange(item.value)
		if err != nil {
			return err
		}
		state.Delivery = "failed"
		encoded, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE state_meta SET value=? WHERE key=?`, string(encoded), item.key); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	messageCount, sessionCount, failedCount = deleted, int64(len(expired)), int64(len(failures))
	// Hygiene is separate from canonical cleanup so a later failure cannot erase
	// the committed mutation counts above. This connection uses WAL/NORMAL.
	_, err = s.db.SQL().ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`)
	return err
}

// StartMaintenance begins immediate-then-hourly sweeping. The returned stop
// function cancels and joins work before the profile database can be closed.
func (s *ProfileStore) StartMaintenance() func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			_ = s.SweepProfile(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}
