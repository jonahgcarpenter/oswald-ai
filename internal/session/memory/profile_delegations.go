package memory

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Delegation lifecycle states for async_delegations rows. Terminal states
// never run again; unknown means the owner died mid-run and its side effects
// cannot be proven, so it must not be retried as new work.
const (
	DelegationRunning     = "running"
	DelegationCompleted   = "completed"
	DelegationFailed      = "failed"
	DelegationInterrupted = "interrupted"
	DelegationStalled     = "stalled"
	DelegationUnknown     = "unknown"
)

// Delegation delivery states. pending rows are picked up by the profile's
// delegation dispatcher; delivered rows are terminal receipts.
const (
	DelegationDeliveryPending   = "pending"
	DelegationDeliveryDelivered = "delivered"
	DelegationDeliveryFailed    = "failed"
)

// DelegationRow mirrors one async_delegations row. task_json carries the full
// dispatch envelope (origin identity, model routing, inputs); result_json
// carries the terminal outcome. Both stay opaque to SQL.
type DelegationRow struct {
	ID               string
	OriginSession    string
	OriginUISession  string
	ParentSessionID  string
	State            string
	DispatchedAt     int64
	CompletedAt      int64
	HasCompletedAt   bool
	UpdatedAt        int64
	EventJSON        string
	ResultJSON       string
	DeliveryState    string
	DeliveryAttempts int
	HasDeliveredAt   bool
	DeliveredAt      int64
	OwnerPID         int64
	HasOwner         bool
	OwnerStartedAt   int64
	TaskJSON         string
	DeliveryClaim    string
	HasDeliveryClaim bool
	DeliveryClaimed  int64
	OriginSessionID  string
}

func scanDelegationRow(row *sql.Rows) (DelegationRow, error) {
	var out DelegationRow
	var dispatchedAt, completedAt, updatedAt, deliveredAt sql.NullFloat64
	var ownerPID sql.NullInt64
	var ownerStartedAt, deliveryClaimedAt sql.NullFloat64
	var parentSession, eventJSON, resultJSON, deliveryClaim sql.NullString
	err := row.Scan(
		&out.ID, &out.OriginSession, &out.OriginUISession, &parentSession,
		&out.State, &dispatchedAt, &completedAt, &updatedAt,
		&eventJSON, &resultJSON, &out.DeliveryState, &out.DeliveryAttempts,
		&deliveredAt, &ownerPID, &ownerStartedAt, &out.TaskJSON,
		&deliveryClaim, &deliveryClaimedAt, &out.OriginSessionID,
	)
	if err != nil {
		return DelegationRow{}, err
	}
	out.DispatchedAt = int64(dispatchedAt.Float64)
	out.UpdatedAt = int64(updatedAt.Float64)
	out.HasCompletedAt = completedAt.Valid
	out.CompletedAt = int64(completedAt.Float64)
	out.HasDeliveredAt = deliveredAt.Valid
	out.DeliveredAt = int64(deliveredAt.Float64)
	out.HasOwner = ownerPID.Valid && ownerPID.Int64 != 0
	out.OwnerPID = ownerPID.Int64
	out.OwnerStartedAt = int64(ownerStartedAt.Float64)
	out.HasDeliveryClaim = deliveryClaim.Valid
	out.DeliveryClaim = deliveryClaim.String
	out.DeliveryClaimed = int64(deliveryClaimedAt.Float64)
	out.ParentSessionID = parentSession.String
	out.EventJSON = eventJSON.String
	out.ResultJSON = resultJSON.String
	return out, nil
}

// InsertDelegation records one dispatched unit as running with no owner. The
// worker claims it before executing; crash recovery reaps never-claimed rows.
func (s *ProfileStore) InsertDelegation(ctx context.Context, row DelegationRow) (resultErr error) {
	started := time.Now()
	defer s.measure("memory.profile.delegation.complete", started, &resultErr)
	if s == nil || s.db == nil {
		return errors.New("profile store is unavailable")
	}
	if row.ID == "" || row.OriginSession == "" || row.TaskJSON == "" {
		return errors.New("invalid delegation scope")
	}
	if row.State == "" {
		row.State = DelegationRunning
	}
	if row.DeliveryState == "" {
		row.DeliveryState = DelegationDeliveryPending
	}
	now := float64(s.now().UnixNano()) / 1e9
	_, resultErr = s.db.SQL().ExecContext(ctx, `INSERT INTO async_delegations(delegation_id,origin_session,origin_ui_session_id,parent_session_id,state,dispatched_at,updated_at,event_json,result_json,delivery_state,delivery_attempts,task_json,origin_session_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		row.ID, row.OriginSession, row.OriginUISession, nullString(row.ParentSessionID), row.State, now, now, nullString(row.EventJSON), nullString(row.ResultJSON), row.DeliveryState, 0, row.TaskJSON, row.OriginSessionID)
	return resultErr
}

// ClaimDelegation takes ownership of a running, unclaimed row. Exactly one
// claimant wins; everyone else proceeds as if the row were busy.
func (s *ProfileStore) ClaimDelegation(ctx context.Context, id string, pid, startedAt float64) (claimed bool, resultErr error) {
	started := time.Now()
	defer s.measure("memory.profile.delegation.complete", started, &resultErr)
	if s == nil || s.db == nil || pid <= 0 {
		return false, errors.New("profile store is unavailable")
	}
	res, err := s.db.SQL().ExecContext(ctx, `UPDATE async_delegations SET owner_pid=?,owner_started_at=?,updated_at=? WHERE delegation_id=? AND state=? AND (owner_pid IS NULL OR owner_pid=0)`, pid, startedAt, s.now().Unix(), id, DelegationRunning)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// FinishDelegation records a terminal outcome for a row owned by the caller.
// Rows owned by someone else are left untouched.
func (s *ProfileStore) FinishDelegation(ctx context.Context, id string, ownerPID int64, state, resultJSON string) (resultErr error) {
	started := time.Now()
	defer s.measure("memory.profile.delegation.complete", started, &resultErr)
	if s == nil || s.db == nil {
		return errors.New("profile store is unavailable")
	}
	switch state {
	case DelegationCompleted, DelegationFailed, DelegationInterrupted, DelegationStalled, DelegationUnknown:
	default:
		return errors.New("invalid delegation state")
	}
	now := float64(s.now().UnixNano()) / 1e9
	_, resultErr = s.db.SQL().ExecContext(ctx, `UPDATE async_delegations SET state=?,result_json=?,completed_at=?,updated_at=? WHERE delegation_id=? AND owner_pid=?`, state, resultJSON, now, now, id, ownerPID)
	return resultErr
}

// ClaimDelivery takes one pending delivery for the dispatcher. Crashed
// dispatchers leave the claim behind; recovery clears stale claims.
func (s *ProfileStore) ClaimDelivery(ctx context.Context, id, token string, now float64) (claimed bool, resultErr error) {
	started := time.Now()
	defer s.measure("memory.profile.delegation.delivery", started, &resultErr)
	if s == nil || s.db == nil || token == "" {
		return false, errors.New("profile store is unavailable")
	}
	res, err := s.db.SQL().ExecContext(ctx, `UPDATE async_delegations SET delivery_claim=?,delivery_claimed_at=? WHERE delegation_id=? AND delivery_state=?`, token, now, id, DelegationDeliveryPending)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// MarkDelivered records a successful handoff to the conversation.
func (s *ProfileStore) MarkDelivered(ctx context.Context, id string, now float64) (resultErr error) {
	started := time.Now()
	defer s.measure("memory.profile.delegation.delivery", started, &resultErr)
	if s == nil || s.db == nil {
		return errors.New("profile store is unavailable")
	}
	_, resultErr = s.db.SQL().ExecContext(ctx, `UPDATE async_delegations SET delivery_state=?,delivered_at=?,delivery_claim=NULL,delivery_claimed_at=NULL,updated_at=? WHERE delegation_id=?`, DelegationDeliveryDelivered, now, now, id)
	return resultErr
}

// NoteDeliveryAttempt counts a failed handoff without changing its pending
// state so the dispatcher retries it.
func (s *ProfileStore) NoteDeliveryAttempt(ctx context.Context, id string, attempts int, now float64) (resultErr error) {
	started := time.Now()
	defer s.measure("memory.profile.delegation.delivery", started, &resultErr)
	if s == nil || s.db == nil {
		return errors.New("profile store is unavailable")
	}
	_, resultErr = s.db.SQL().ExecContext(ctx, `UPDATE async_delegations SET delivery_attempts=?,delivery_claim=NULL,delivery_claimed_at=NULL,updated_at=? WHERE delegation_id=?`, attempts, now, id)
	return resultErr
}

// FailDelivery parks a row that exhausted its handoff budget.
func (s *ProfileStore) FailDelivery(ctx context.Context, id string, attempts int, now float64) (resultErr error) {
	started := time.Now()
	defer s.measure("memory.profile.delegation.delivery", started, &resultErr)
	if s == nil || s.db == nil {
		return errors.New("profile store is unavailable")
	}
	_, resultErr = s.db.SQL().ExecContext(ctx, `UPDATE async_delegations SET delivery_state=?,delivery_attempts=?,delivery_claim=NULL,delivery_claimed_at=NULL,updated_at=? WHERE delegation_id=?`, DelegationDeliveryFailed, attempts, now, id)
	return resultErr
}

// ListDueDeliveries returns terminal rows still awaiting handoff, oldest
// first and bounded for one dispatcher pass.
func (s *ProfileStore) ListDueDeliveries(ctx context.Context, limit int) (rows []DelegationRow, resultErr error) {
	started := time.Now()
	defer s.measure("memory.profile.delegation.delivery", started, &resultErr)
	if s == nil || s.db == nil {
		return nil, errors.New("profile store is unavailable")
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	query, err := s.db.SQL().QueryContext(ctx, `SELECT delegation_id,origin_session,origin_ui_session_id,COALESCE(parent_session_id,''),state,dispatched_at,completed_at,updated_at,COALESCE(event_json,''),COALESCE(result_json,''),delivery_state,delivery_attempts,delivered_at,owner_pid,owner_started_at,COALESCE(task_json,''),delivery_claim,delivery_claimed_at,origin_session_id FROM async_delegations WHERE delivery_state=? AND state IN ('completed','failed','interrupted','stalled') ORDER BY completed_at ASC LIMIT ?`, DelegationDeliveryPending, limit)
	if err != nil {
		return nil, err
	}
	defer query.Close()
	for query.Next() {
		row, err := scanDelegationRow(query)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, query.Err()
}

// PruneDelegations deletes delivered and terminal rows older than the given
// age, then enforces the total row cap by dropping the oldest terminal rows
// first. Live and pending rows are never touched.
func (s *ProfileStore) PruneDelegations(ctx context.Context, olderThanUnix int64, maxRows int) (deleted int64, resultErr error) {
	started := time.Now()
	defer s.measure("memory.profile.delegation.delivery", started, &resultErr)
	if s == nil || s.db == nil {
		return 0, errors.New("profile store is unavailable")
	}
	res, err := s.db.SQL().ExecContext(ctx, `DELETE FROM async_delegations WHERE delivery_state IN (?,?) AND updated_at<?`, DelegationDeliveryDelivered, DelegationDeliveryFailed, olderThanUnix)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	deleted += n
	if maxRows > 0 {
		res, err = s.db.SQL().ExecContext(ctx, `DELETE FROM async_delegations WHERE delegation_id IN (SELECT delegation_id FROM async_delegations WHERE state<>? AND delivery_state IN (?,?) ORDER BY updated_at ASC LIMIT MAX(0,(SELECT COUNT(*) FROM async_delegations)-?))`, DelegationRunning, DelegationDeliveryDelivered, DelegationDeliveryFailed, maxRows)
		if err != nil {
			return deleted, err
		}
		n, err = res.RowsAffected()
		if err != nil {
			return deleted, err
		}
		deleted += n
	}
	return deleted, nil
}

// ReapOrphanedDelegations marks running rows whose owner is gone as unknown.
// Rows never claimed count as orphaned too: nobody owns their side effects,
// so they must never be retried as new work.
func (s *ProfileStore) ReapOrphanedDelegations(ctx context.Context, alive func(pid int64) bool, now float64) (reaped int64, resultErr error) {
	started := time.Now()
	defer s.measure("memory.profile.delegation.complete", started, &resultErr)
	if s == nil || s.db == nil {
		return 0, errors.New("profile store is unavailable")
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT delegation_id,origin_session,origin_ui_session_id,COALESCE(parent_session_id,''),state,dispatched_at,completed_at,updated_at,COALESCE(event_json,''),COALESCE(result_json,''),delivery_state,delivery_attempts,delivered_at,owner_pid,owner_started_at,COALESCE(task_json,''),delivery_claim,delivery_claimed_at,origin_session_id FROM async_delegations WHERE state=?`, DelegationRunning)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		row, err := scanDelegationRow(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		if !row.HasOwner || !alive(row.OwnerPID) {
			ids = append(ids, row.ID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := s.db.SQL().ExecContext(ctx, `UPDATE async_delegations SET state=?,updated_at=? WHERE delegation_id=? AND state=?`, DelegationUnknown, now, id, DelegationRunning); err != nil {
			return reaped, err
		}
		reaped++
	}
	return reaped, nil
}
