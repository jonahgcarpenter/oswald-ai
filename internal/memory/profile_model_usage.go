package memory

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// ModelUsageRecord attributes one completed provider call to a profile
// session. SessionID is the conversation key and UserID the profile owner;
// Generation fences the write to the active session, like message
// persistence. Costs are intentionally left NULL: no cost engine exists yet.
type ModelUsageRecord struct {
	SessionID, UserID                                                                            string
	Generation                                                                                   int
	Model, BillingProvider, BillingBaseURL, BillingMode, Task                                    string
	ApiCalls, PromptTokens, CompletionTokens, CacheReadTokens, CacheWriteTokens, ReasoningTokens int
}

// RecordModelUsage upserts one provider call into session_model_usage,
// accumulating counters across calls. It returns sql.ErrNoRows when the
// generation is no longer the active session, leaving existing rows unchanged.
func (s *ProfileStore) RecordModelUsage(ctx context.Context, record ModelUsageRecord) (resultErr error) {
	defer s.measure("memory.profile.usage.complete", time.Now(), &resultErr)
	if record.UserID != s.profile {
		return errors.New("invalid profile usage scope")
	}
	if _, err := s.scope(record.UserID, record.SessionID); err != nil {
		return err
	}
	if record.Generation <= 0 {
		return errors.New("invalid profile usage generation")
	}
	model := strings.TrimSpace(record.Model)
	if !utf8.ValidString(model) || model == "" || len(model) > 256 {
		return errors.New("invalid profile usage model")
	}
	for _, field := range []string{record.BillingProvider, record.BillingBaseURL, record.BillingMode} {
		if !utf8.ValidString(field) || len(field) > 512 {
			return errors.New("invalid profile usage billing scope")
		}
	}
	if !utf8.ValidString(record.Task) || len(record.Task) > 128 {
		return errors.New("invalid profile usage task")
	}
	for _, count := range []int{record.ApiCalls, record.PromptTokens, record.CompletionTokens, record.CacheReadTokens, record.CacheWriteTokens, record.ReasoningTokens} {
		if count < 0 {
			return errors.New("invalid profile usage counters")
		}
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	id, err := s.activeSession(ctx, tx, record.UserID, record.SessionID, record.Generation)
	if err != nil {
		return err
	}
	seconds := float64(s.now().UTC().UnixNano()) / 1e9
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_model_usage(session_id,model,billing_provider,billing_base_url,billing_mode,task,api_call_count,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,first_seen,last_seen)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(session_id,model,billing_provider,billing_base_url,billing_mode,task) DO UPDATE SET
 api_call_count=api_call_count+excluded.api_call_count,
 input_tokens=input_tokens+excluded.input_tokens,
 output_tokens=output_tokens+excluded.output_tokens,
 cache_read_tokens=cache_read_tokens+excluded.cache_read_tokens,
 cache_write_tokens=cache_write_tokens+excluded.cache_write_tokens,
 reasoning_tokens=reasoning_tokens+excluded.reasoning_tokens,
 last_seen=excluded.last_seen`,
		id, model, record.BillingProvider, record.BillingBaseURL, record.BillingMode, record.Task,
		record.ApiCalls, record.PromptTokens, record.CompletionTokens, record.CacheReadTokens, record.CacheWriteTokens, record.ReasoningTokens,
		seconds, seconds); err != nil {
		return err
	}
	return tx.Commit()
}

// ModelUsageRow is a read-friendly projection of one session_model_usage row.
type ModelUsageRow struct {
	SessionID, Model, BillingProvider, BillingBaseURL, BillingMode, Task                         string
	ApiCalls, PromptTokens, CompletionTokens, CacheReadTokens, CacheWriteTokens, ReasoningTokens int
	FirstSeen, LastSeen                                                                          float64
}

// SessionModelUsage lists one session's persisted usage ledger in key order.
// It is a test and inspection seam; production history never depends on it.
func (s *ProfileStore) SessionModelUsage(ctx context.Context, owner, key string, generation int) ([]ModelUsageRow, error) {
	if owner != s.profile {
		return nil, errors.New("invalid profile usage scope")
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	id, err := s.activeSession(ctx, tx, owner, key, generation)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT model,billing_provider,billing_base_url,billing_mode,task,api_call_count,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,first_seen,last_seen FROM session_model_usage WHERE session_id=? ORDER BY model,task`, id)
	if err != nil {
		return nil, err
	}
	var usage []ModelUsageRow
	for rows.Next() {
		var row ModelUsageRow
		row.SessionID = id
		if err := rows.Scan(&row.Model, &row.BillingProvider, &row.BillingBaseURL, &row.BillingMode, &row.Task, &row.ApiCalls, &row.PromptTokens, &row.CompletionTokens, &row.CacheReadTokens, &row.CacheWriteTokens, &row.ReasoningTokens, &row.FirstSeen, &row.LastSeen); err != nil {
			rows.Close()
			return nil, err
		}
		usage = append(usage, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return usage, tx.Commit()
}
