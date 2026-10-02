package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strconv"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// ProfileCompression is an exact-token, fixed-range durable compression claim.
type ProfileCompression struct {
	Scope          ActiveSessionScope
	SessionRowID   string
	Token          string
	From, Through  int64
	Contract       string
	Target         int64
	Artifact       *SummaryArtifact
	CorrectiveCode string
}
type profileCompressionReceipt struct {
	Version        int              `json:"version"`
	Submissions    int              `json:"submissions"`
	State          string           `json:"state"`
	UpdatedAt      float64          `json:"updated_at"`
	SourceIDs      []int64          `json:"source_ids"`
	Target         int64            `json:"target"`
	Artifact       *SummaryArtifact `json:"artifact,omitempty"`
	CorrectiveCode string           `json:"corrective_code,omitempty"`
}

type profileCompressionCampaign struct {
	Version int   `json:"version"`
	Target  int64 `json:"target"`
}

func compressionCampaignKey(id, contract string) string {
	return "oswald:v1:campaign:" + id + ":" + contract
}

func readCompressionReceipt(ctx context.Context, tx *sql.Tx, work ProfileCompression) (profileCompressionReceipt, error) {
	var encoded string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM state_meta WHERE key=?`, compressionReceiptKey(work)).Scan(&encoded); err != nil {
		return profileCompressionReceipt{}, err
	}
	var receipt profileCompressionReceipt
	if len(encoded) > 64*1024 || json.Unmarshal([]byte(encoded), &receipt) != nil || receipt.Version != 1 || receipt.Submissions < 0 || receipt.Submissions > 4 {
		return receipt, errors.New("invalid compression receipt")
	}
	return receipt, nil
}

// CompressionCampaignTarget returns the frozen high-water checkpoint target.
// A campaign continues to that target even when newer prompt pressure falls.
func (s *ProfileStore) CompressionCampaignTarget(ctx context.Context, scope ActiveSessionScope, contract string) (int64, error) {
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	id, err := s.activeSession(ctx, tx, scope.UserID, scope.SessionID, scope.Generation)
	if err != nil {
		return 0, err
	}
	var encoded string
	err = tx.QueryRowContext(ctx, `SELECT value FROM state_meta WHERE key=?`, compressionCampaignKey(id, contract)).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var campaign profileCompressionCampaign
	if json.Unmarshal([]byte(encoded), &campaign) != nil || campaign.Version != 1 || campaign.Target <= 0 {
		return 0, errors.New("invalid compression campaign")
	}
	return campaign.Target, tx.Commit()
}

func compressionReceiptKey(work ProfileCompression) string {
	return "oswald:v1:compression:" + work.SessionRowID + ":" + strconv.FormatInt(work.From, 10) + ":" + strconv.FormatInt(work.Through, 10) + ":" + work.Contract
}

// CompressionRetryThrough keeps an existing range stable when newer exchanges
// arrive. Terminal receipts also retain their range to suppress failed work.
func (s *ProfileStore) CompressionRetryThrough(ctx context.Context, scope ActiveSessionScope, from int64, contract string) (int64, error) {
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	id, err := s.activeSession(ctx, tx, scope.UserID, scope.SessionID, scope.Generation)
	if err != nil {
		return 0, err
	}
	prefix := "oswald:v1:compression:" + id + ":" + strconv.FormatInt(from, 10) + ":"
	var encoded string
	err = tx.QueryRowContext(ctx, `SELECT value FROM state_meta WHERE substr(key,1,?)=? AND substr(key,-?)=? ORDER BY key LIMIT 1`, len(prefix), prefix, len(contract)+1, ":"+contract).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var receipt profileCompressionReceipt
	if len(encoded) > 64*1024 || json.Unmarshal([]byte(encoded), &receipt) != nil || receipt.Version != 1 || len(receipt.SourceIDs) == 0 || len(receipt.SourceIDs) > 64 || receipt.SourceIDs[0] != from {
		return 0, errors.New("invalid compression retry range")
	}
	return receipt.SourceIDs[len(receipt.SourceIDs)-1], tx.Commit()
}

// CompressionScopes enumerates active profile conversations in bounded pages.
func (s *ProfileStore) CompressionScopes(ctx context.Context) ([]ActiveSessionScope, error) {
	return s.CompressionScopesAfter(ctx, "")
}

// CompressionScopesAfter selects the next bounded lexical page of active
// conversation keys. A worker cursor prevents busy recent chats starving others.
func (s *ProfileStore) CompressionScopesAfter(ctx context.Context, after string) ([]ActiveSessionScope, error) {
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT session_key,json_extract(origin_json,'$.generation') FROM sessions WHERE profile_name=? AND ended_at IS NULL AND session_key>? AND last_activity_at+json_extract(origin_json,'$.ttl_seconds')>? ORDER BY session_key LIMIT 100`, s.profile, after, float64(s.now().UnixNano())/1e9)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var scopes []ActiveSessionScope
	for rows.Next() {
		scope := ActiveSessionScope{UserID: s.profile}
		if err := rows.Scan(&scope.SessionID, &scope.Generation); err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	return scopes, rows.Err()
}

// CompressionHealth reports current durable receipt and lease gauges without
// exposing source text, session keys, artifacts, or model-authored values.
func (s *ProfileStore) CompressionHealth(ctx context.Context) (retry, ready, dead, done, expired int64, resultErr error) {
	err := s.db.SQL().QueryRowContext(ctx, `SELECT
 COALESCE(SUM(json_extract(value,'$.state')='retry' AND json_extract(value,'$.submissions')<4),0),
 COALESCE(SUM(json_extract(value,'$.state')='ready'),0),
 COALESCE(SUM(json_extract(value,'$.state')='dead' OR (json_extract(value,'$.state')='retry' AND json_extract(value,'$.submissions')>=4)),0),
 COALESCE(SUM(json_extract(value,'$.state')='done'),0)
 FROM state_meta WHERE substr(key,1,22)='oswald:v1:compression:'`).Scan(&retry, &ready, &dead, &done)
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	err = s.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM compression_locks WHERE expires_at<=?`, float64(s.now().UnixNano())/1e9).Scan(&expired)
	return retry, ready, dead, done, expired, err
}

// CompressionCandidates stops before pending delivery, but may pass failed
// sends. Eligibility is rechecked transactionally at publication.
func (s *ProfileStore) CompressionCandidates(ctx context.Context, scope ActiveSessionScope, after int64) ([]SessionTurn, SessionPromptPressure, error) {
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return nil, SessionPromptPressure{}, err
	}
	defer tx.Rollback()
	id, err := s.activeSession(ctx, tx, scope.UserID, scope.SessionID, scope.Generation)
	if err != nil {
		return nil, SessionPromptPressure{}, err
	}
	var barrier int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(m.id),9223372036854775807) FROM messages m JOIN state_meta v ON v.key=('oswald:v1:turn:'||m.session_id||':'||m.id) WHERE m.session_id=? AND m.id>? AND json_extract(v.value,'$.delivery')='pending'`, id, after).Scan(&barrier); err != nil {
		return nil, SessionPromptPressure{}, err
	}
	var encoded string
	err = tx.QueryRowContext(ctx, `SELECT v.value FROM messages m JOIN state_meta v ON v.key=('oswald:v1:turn:'||m.session_id||':'||m.id) WHERE m.session_id=? AND m.id>? AND m.id<? AND m.active=1 AND json_extract(v.value,'$.delivery')='delivered' ORDER BY m.id DESC LIMIT 1`, id, after, barrier).Scan(&encoded)
	if err != nil {
		return nil, SessionPromptPressure{}, err
	}
	state, err := decodeProfileExchange(encoded)
	if err != nil {
		return nil, SessionPromptPressure{}, err
	}
	if err := tx.Commit(); err != nil {
		return nil, SessionPromptPressure{}, err
	}
	turns, err := s.PageDeliveredSessionTurnsAfter(ctx, scope.UserID, scope.SessionID, scope.Generation, after, 64)
	if err != nil {
		return nil, SessionPromptPressure{}, err
	}
	count := 0
	for count < len(turns) && turns[count].ID < barrier {
		count++
	}
	return turns[:count], state.Pressure, nil
}

// ClaimCompression excludes concurrent compressors and freezes a source range.
func (s *ProfileStore) ClaimCompression(ctx context.Context, scope ActiveSessionScope, turns []SessionTurn, contract string) (ProfileCompression, error) {
	if len(turns) == 0 || len(turns) > 64 || len(contract) > 512 {
		return ProfileCompression{}, errors.New("invalid compression range")
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return ProfileCompression{}, err
	}
	defer tx.Rollback()
	id, err := s.activeSession(ctx, tx, scope.UserID, scope.SessionID, scope.Generation)
	if err != nil {
		return ProfileCompression{}, err
	}
	work := ProfileCompression{Scope: scope, SessionRowID: id, Token: config.NewRequestID(), From: turns[0].ID, Through: turns[len(turns)-1].ID, Contract: contract}
	var sourceIDs []int64
	for _, turn := range turns {
		if turn.ID <= 0 || (len(sourceIDs) > 0 && turn.ID <= sourceIDs[len(sourceIDs)-1]) {
			return work, errors.New("invalid compression source order")
		}
		sourceIDs = append(sourceIDs, turn.ID)
	}
	receipt, err := readCompressionReceipt(ctx, tx, work)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return work, err
	}
	if err == nil {
		if !reflect.DeepEqual(receipt.SourceIDs, sourceIDs) {
			return work, errors.New("compression sources changed")
		}
		if receipt.State == "dead" || receipt.State == "done" || (receipt.Submissions >= 4 && receipt.Artifact == nil) {
			return work, sql.ErrNoRows
		}
		work.Target, work.Artifact, work.CorrectiveCode = receipt.Target, receipt.Artifact, receipt.CorrectiveCode
	} else {
		var encoded string
		err := tx.QueryRowContext(ctx, `SELECT value FROM state_meta WHERE key=?`, compressionCampaignKey(id, contract)).Scan(&encoded)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return work, err
		}
		campaign := profileCompressionCampaign{Version: 1}
		if err == nil {
			if json.Unmarshal([]byte(encoded), &campaign) != nil || campaign.Version != 1 {
				return work, errors.New("invalid compression campaign")
			}
		} else {
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(m.id),0) FROM messages m JOIN state_meta v ON v.key=('oswald:v1:turn:'||m.session_id||':'||m.id) WHERE m.session_id=? AND m.active=1 AND json_extract(v.value,'$.delivery')='delivered' AND m.id<COALESCE((SELECT MIN(p.id) FROM messages p JOIN state_meta pv ON pv.key=('oswald:v1:turn:'||p.session_id||':'||p.id) WHERE p.session_id=? AND p.id>=? AND json_extract(pv.value,'$.delivery')='pending'),9223372036854775807)`, id, id, work.From).Scan(&campaign.Target); err != nil {
				return work, err
			}
			encoded, err := json.Marshal(campaign)
			if err != nil {
				return work, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO state_meta(key,value) VALUES(?,?)`, compressionCampaignKey(id, contract), string(encoded)); err != nil {
				return work, err
			}
		}
		if campaign.Target < work.Through {
			return work, errors.New("compression range exceeds campaign")
		}
		work.Target = campaign.Target
		receipt = profileCompressionReceipt{Version: 1, State: "retry", SourceIDs: sourceIDs, Target: work.Target, UpdatedAt: float64(s.now().UnixNano()) / 1e9}
		encodedReceipt, err := json.Marshal(receipt)
		if err != nil {
			return work, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO state_meta(key,value) VALUES(?,?)`, compressionReceiptKey(work), string(encodedReceipt)); err != nil {
			return work, err
		}
	}
	now := float64(s.now().UnixNano()) / 1e9
	result, err := tx.ExecContext(ctx, `INSERT INTO compression_locks(session_id,holder,acquired_at,expires_at) VALUES(?,?,?,?) ON CONFLICT(session_id) DO UPDATE SET holder=excluded.holder,acquired_at=excluded.acquired_at,expires_at=excluded.expires_at WHERE compression_locks.expires_at<=?`, id, work.Token, now, now+300, now)
	if err != nil {
		return work, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return work, err
	}
	if count != 1 {
		return work, sql.ErrNoRows
	}
	return work, tx.Commit()
}

func (s *ProfileStore) requireCompression(ctx context.Context, tx *sql.Tx, work ProfileCompression) error {
	id, err := s.activeSession(ctx, tx, work.Scope.UserID, work.Scope.SessionID, work.Scope.Generation)
	if err != nil {
		return err
	}
	if id != work.SessionRowID {
		return ErrStaleSessionCompactionJobLease
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM compression_locks WHERE session_id=? AND holder=? AND expires_at>?`, id, work.Token, float64(s.now().UnixNano())/1e9).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return ErrStaleSessionCompactionJobLease
	}
	return nil
}

// RenewCompression replaces the exact token so stale pre-renewal callers fail.
func (s *ProfileStore) RenewCompression(ctx context.Context, work ProfileCompression) (ProfileCompression, error) {
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return work, err
	}
	defer tx.Rollback()
	if err := s.requireCompression(ctx, tx, work); err != nil {
		return work, err
	}
	next := work
	next.Token = config.NewRequestID()
	if _, err := tx.ExecContext(ctx, `UPDATE compression_locks SET holder=?,expires_at=? WHERE session_id=? AND holder=?`, next.Token, float64(s.now().UnixNano())/1e9+300, work.SessionRowID, work.Token); err != nil {
		return work, err
	}
	return next, tx.Commit()
}

// ReserveCompressionSubmission commits one of four durable provider credits.
func (s *ProfileStore) ReserveCompressionSubmission(ctx context.Context, work ProfileCompression) error {
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.requireCompression(ctx, tx, work); err != nil {
		return err
	}
	receipt, err := readCompressionReceipt(ctx, tx, work)
	if err != nil {
		return err
	}
	if receipt.Artifact != nil {
		return errors.New("compression artifact already saved")
	}
	if receipt.Submissions >= 4 || receipt.State == "dead" || receipt.State == "done" {
		return ErrModelSubmissionBudgetExhausted
	}
	receipt.Submissions++
	receipt.UpdatedAt = float64(s.now().UnixNano()) / 1e9
	encodedBytes, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO state_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, compressionReceiptKey(work), string(encodedBytes)); err != nil {
		return err
	}
	return tx.Commit()
}

// SaveCompressionArtifact freezes validated output before publication so a
// restart or storage retry never spends another provider credit for that result.
func (s *ProfileStore) SaveCompressionArtifact(ctx context.Context, work ProfileCompression, artifact SummaryArtifact) error {
	_, normalized, err := encodeSummaryArtifact(artifact)
	if err != nil {
		return err
	}
	if len(normalized.Candidates) != 0 {
		return errors.New("profile summaries cannot publish memory candidates")
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.requireCompression(ctx, tx, work); err != nil {
		return err
	}
	receipt, err := readCompressionReceipt(ctx, tx, work)
	if err != nil {
		return err
	}
	if receipt.Submissions == 0 {
		return errors.New("compression artifact has no submission")
	}
	if receipt.Artifact != nil {
		if !reflect.DeepEqual(*receipt.Artifact, normalized) {
			return errors.New("compression artifact changed")
		}
		return tx.Commit()
	}
	receipt.Artifact = &normalized
	receipt.State = "ready"
	receipt.UpdatedAt = float64(s.now().UnixNano()) / 1e9
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if len(encoded) > 64*1024 {
		return errors.New("compression receipt exceeds limit")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE state_meta SET value=? WHERE key=?`, string(encoded), compressionReceiptKey(work)); err != nil {
		return err
	}
	return tx.Commit()
}

var compressionCode = regexp.MustCompile(`^[a-z_]{1,64}$`)

// RecordCompressionCorrection persists a developer-owned validation reason,
// never a raw provider error, for the next one of the four allowed submissions.
func (s *ProfileStore) RecordCompressionCorrection(ctx context.Context, work ProfileCompression, code string) error {
	if !compressionCode.MatchString(code) {
		return errors.New("invalid compression correction code")
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.requireCompression(ctx, tx, work); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE state_meta SET value=json_set(value,'$.corrective_code',?) WHERE key=?`, code, compressionReceiptKey(work)); err != nil {
		return err
	}
	return tx.Commit()
}

// PublishProfileSummary checks the exact live lease and every delivered source
// before atomically publishing a structured checkpoint and completion receipt.
func (s *ProfileStore) PublishProfileSummary(ctx context.Context, work ProfileCompression, turns []SessionTurn, artifact SummaryArtifact) error {
	_, artifact, err := encodeSummaryArtifact(artifact)
	if err != nil {
		return err
	}
	if len(artifact.Candidates) != 0 || len(turns) == 0 || turns[0].ID != work.From || turns[len(turns)-1].ID != work.Through {
		return errors.New("invalid profile summary range")
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.requireCompression(ctx, tx, work); err != nil {
		return err
	}
	receipt, err := readCompressionReceipt(ctx, tx, work)
	if err != nil {
		return err
	}
	if receipt.Artifact == nil || !reflect.DeepEqual(*receipt.Artifact, artifact) {
		return errors.New("compression artifact was not durably saved")
	}
	var sourceIDs []int64
	for _, turn := range turns {
		sourceIDs = append(sourceIDs, turn.ID)
	}
	if !reflect.DeepEqual(receipt.SourceIDs, sourceIDs) {
		return errors.New("compression sources changed")
	}
	// Do not overwrite a newer checkpoint or silently omit an earlier delivered
	// exchange. This also fences publication after a repaired delivery.
	var previousThrough int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT json_extract(value,'$.summary.CoveredThroughTurnID') FROM state_meta WHERE key=?),0)`, profileSummaryKey(work.SessionRowID)).Scan(&previousThrough); err != nil {
		return err
	}
	if previousThrough >= work.From {
		return errors.New("compression checkpoint already advanced")
	}
	var eligibleCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages m JOIN state_meta v ON v.key=('oswald:v1:turn:'||m.session_id||':'||m.id) WHERE m.session_id=? AND m.id>? AND m.id<=? AND m.active=1 AND json_extract(v.value,'$.delivery')='delivered'`, work.SessionRowID, previousThrough, work.Through).Scan(&eligibleCount); err != nil {
		return err
	}
	if eligibleCount != len(turns) {
		return errors.New("compression range omitted delivered sources")
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages m JOIN state_meta v ON v.key=('oswald:v1:turn:'||m.session_id||':'||m.id) WHERE m.session_id=? AND m.id>=? AND m.id<=? AND json_extract(v.value,'$.delivery')='pending'`, work.SessionRowID, work.From, work.Through).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return errors.New("pending delivery blocks profile summary")
	}
	summary := SessionSummary{ID: work.Through, UserID: work.Scope.UserID, SessionID: work.Scope.SessionID, SessionGeneration: work.Scope.Generation, CoveredFromTurnID: work.From, CoveredThroughTurnID: work.Through, Narrative: artifact.Narrative, OpenTasks: artifact.OpenTasks, Commitments: artifact.Commitments, Entities: artifact.Entities, Decisions: artifact.Decisions, TopicTags: artifact.TopicTags}
	for _, turn := range turns {
		var content, answer string
		if err := tx.QueryRowContext(ctx, `SELECT u.content,a.content FROM messages a JOIN state_meta v ON v.key=('oswald:v1:turn:'||a.session_id||':'||a.id) JOIN messages u ON u.id=json_extract(v.value,'$.user_message_id') AND u.session_id=a.session_id AND u.active=1 WHERE a.id=? AND a.session_id=? AND a.active=1 AND json_extract(v.value,'$.delivery')='delivered'`, turn.ID, work.SessionRowID).Scan(&content, &answer); err != nil {
			return err
		}
		if content != turn.UserText || answer != turn.AssistantText {
			return errors.New("profile summary source changed")
		}
		summary.SourceTurnIDs = append(summary.SourceTurnIDs, turn.ID)
	}
	encoded, err := json.Marshal(profileSummary{Version: 1, Summary: summary})
	if err != nil {
		return err
	}
	if len(encoded) > 64*1024 {
		return errors.New("profile summary exceeds limit")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO state_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, profileSummaryKey(work.SessionRowID), string(encoded)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE state_meta SET value=json_set(value,'$.state','done') WHERE key=?`, compressionReceiptKey(work)); err != nil {
		return err
	}
	if work.Through >= work.Target {
		if _, err := tx.ExecContext(ctx, `DELETE FROM state_meta WHERE key=?`, compressionCampaignKey(work.SessionRowID, work.Contract)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReleaseCompression preserves retry credits, optionally refunds a preempted
// attempt, and never releases another claimant's lease, even after expiry.
func (s *ProfileStore) ReleaseCompression(ctx context.Context, work ProfileCompression, refund, dead bool) error {
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM compression_locks WHERE session_id=? AND holder=?`, work.SessionRowID, work.Token)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrStaleSessionCompactionJobLease
	}
	if refund {
		if _, err := tx.ExecContext(ctx, `UPDATE state_meta SET value=json_set(value,'$.submissions',MAX(0,json_extract(value,'$.submissions')-1)) WHERE key=?`, compressionReceiptKey(work)); err != nil {
			return err
		}
	}
	if dead {
		if _, err := tx.ExecContext(ctx, `UPDATE state_meta SET value=json_set(value,'$.state','dead') WHERE key=?`, compressionReceiptKey(work)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CompressionContract includes the prompt-capacity policy in retry identity.
func CompressionContract(model, version string, limit int) string {
	return model + ":" + version + ":" + strconv.Itoa(limit)
}
