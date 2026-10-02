package compaction

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/lease"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

// LowPriorityGate grants cancelable model capacity only while foreground work is idle.
type LowPriorityGate interface {
	TryAcquireLowPriority(context.Context) (context.Context, func(), bool)
}

// ProfileService owns one profile's post-delivery compression worker. Retry
// credits and checkpoints survive restart in the profile's state_meta records.
type ProfileService struct {
	store     *memory.ProfileStore
	compactor *LLMCompactor
	gate      LowPriorityGate
	limit     int
	log       *config.Logger
	wake      chan struct{}
	cancel    context.CancelFunc
	done      chan struct{}
	cursor    string
}

// NewProfileService constructs a worker without opening another database handle.
func NewProfileService(store *memory.ProfileStore, compactor *LLMCompactor, gate LowPriorityGate, input budget.ContextBudget, log *config.Logger) *ProfileService {
	return &ProfileService{store: store, compactor: compactor, gate: gate, limit: input.UsableInputLimit(), log: log, wake: make(chan struct{}, 1), done: make(chan struct{})}
}

// Start gives the worker an independent lifetime; Stop joins it before DB close.
func (s *ProfileService) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go func() {
		defer close(s.done)
		timer := time.NewTicker(30 * time.Second)
		defer timer.Stop()
		s.cycle(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				s.cycle(ctx)
			case <-timer.C:
				s.cycle(ctx)
			}
		}
	}()
}

// Stop cancels active provider work and joins bounded bookkeeping.
func (s *ProfileService) Stop() {
	if s.cancel != nil {
		s.cancel()
		<-s.done
	}
}

// Enqueue acknowledges delivery before waking compression planning.
func (s *ProfileService) Enqueue(ctx context.Context, owner string, source memory.FormationSource) error {
	if err := s.store.MarkSessionTurnDelivered(ctx, owner, source.TurnID); err != nil {
		return err
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

// MarkDeliveryFailed keeps the exchange out of context and compression.
func (s *ProfileService) MarkDeliveryFailed(ctx context.Context, owner string, turn int64) error {
	return s.store.MarkSessionTurnDeliveryFailed(ctx, owner, turn)
}

func (s *ProfileService) cycle(ctx context.Context) {
	scopes, err := s.store.CompressionScopesAfter(ctx, s.cursor)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Server("compaction").Warn("compaction.profile.scan_failed", "failed to scan profile compression", config.ErrorField(err))
		}
		return
	}
	if len(scopes) == 100 {
		s.cursor = scopes[len(scopes)-1].SessionID
	} else {
		s.cursor = ""
	}
	healthCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	retry, ready, dead, done, expired, healthErr := s.store.CompressionHealth(healthCtx)
	cancel()
	if healthErr == nil {
		s.log.Server("compaction").Info("compaction.profile.health", "profile compression health", config.F("record_kind", "snapshot"), config.F("retry_count", retry), config.F("ready_count", ready), config.F("dead_count", dead), config.F("succeeded_count", done), config.F("expired_lease_count", expired), config.F("selected_scope_count", len(scopes)), config.F("is_scope_selection_capped", len(scopes) == 100))
	} else if ctx.Err() == nil {
		s.log.Server("compaction").Warn("compaction.profile.health_failed", "failed to read profile compression health", config.ErrorField(healthErr))
	}
	for _, scope := range scopes {
		if ctx.Err() != nil {
			return
		}
		s.runScope(ctx, scope)
	}
}

func (s *ProfileService) runScope(ctx context.Context, scope memory.ActiveSessionScope) {
	previous, err := s.store.LatestSessionSummary(ctx, scope.UserID, scope.SessionID, scope.Generation)
	if err != nil {
		return
	}
	turns, pressure, err := s.store.CompressionCandidates(ctx, scope, previous.CoveredThroughTurnID)
	if err != nil || len(turns) == 0 {
		return
	}
	contract := memory.CompressionContract(s.compactor.model, SummaryGeneratorVersion, s.limit)
	target, err := s.store.CompressionCampaignTarget(ctx, scope, contract)
	if err != nil {
		return
	}
	if target == 0 && (pressure.Limit <= 0 || float64(pressure.Tokens)/float64(pressure.Limit) < 0.7) {
		return
	}
	if target > 0 {
		count := 0
		for count < len(turns) && turns[count].ID <= target {
			count++
		}
		turns = turns[:count]
		if len(turns) == 0 {
			return
		}
	}
	first := turns[0]
	retryThrough, err := s.store.CompressionRetryThrough(ctx, scope, first.ID, contract)
	if err != nil {
		return
	}
	if retryThrough > 0 {
		count := 0
		for count < len(turns) && turns[count].ID <= retryThrough {
			count++
		}
		if count == 0 || turns[count-1].ID != retryThrough {
			return
		}
		turns = turns[:count]
	}
	var prior *memory.SessionSummary
	if previous.ID > 0 {
		prior = &previous
	}
	for len(turns) > 0 {
		messages, err := compactionMessages(prior, turns, "")
		if err != nil {
			return
		}
		if budget.EstimateRequest(messages, []llm.Tool{s.compactor.tool}) <= s.limit {
			break
		}
		// An existing range is immutable. Never create a new receipt to bypass
		// its provider credits or discard an artifact awaiting publication.
		if retryThrough > 0 {
			return
		}
		turns = turns[:len(turns)-1]
	}
	if len(turns) == 0 {
		work, err := s.store.ClaimCompression(ctx, scope, []memory.SessionTurn{first}, contract)
		if err == nil {
			if err := s.store.ReleaseCompression(ctx, work, false, true); err != nil {
				s.log.Server("compaction").Warn("compaction.profile.release_failed", "failed to retire uncompactable exchange", config.ErrorField(err))
				return
			}
			s.log.Server("compaction").Info("compaction.profile.uncompactable", "complete exchange exceeds compression capacity", config.F("record_kind", "measurement"), config.F("user_id", scope.UserID), config.F("turn_count", 1), config.F("status", "rejected"))
		}
		return
	}
	workCtx, release, ok := s.gate.TryAcquireLowPriority(ctx)
	if !ok {
		return
	}
	defer release()
	work, err := s.store.ClaimCompression(workCtx, scope, turns, contract)
	if err != nil {
		return
	}
	started := time.Now()
	submitted := false
	artifactSaved := work.Artifact != nil
	artifactReused := artifactSaved
	savedArtifact, correctiveCode := work.Artifact, work.CorrectiveCode
	outcome, status := "completed", "ok"
	dead, refund := false, false
	meta := requestctx.Metadata{OperationID: config.NewRequestID(), Workload: "compaction"}
	workCtx = requestctx.WithMetadata(workCtx, meta)
	// Background work has canonical ownership but no invented transport identity.
	workCtx = requestctx.WithPrincipal(workCtx, identity.Principal{CanonicalUserID: scope.UserID})
	var mutex sync.Mutex
	err = lease.Run(workCtx, 5*time.Minute, func(ctx context.Context) error {
		mutex.Lock()
		defer mutex.Unlock()
		renewed, err := s.store.RenewCompression(ctx, work)
		if err == nil {
			work = renewed
		}
		return err
	}, func(ctx context.Context) error {
		if savedArtifact != nil {
			mutex.Lock()
			defer mutex.Unlock()
			return s.store.PublishProfileSummary(ctx, work, turns, *savedArtifact)
		}
		mutex.Lock()
		err := s.store.ReserveCompressionSubmission(ctx, work)
		mutex.Unlock()
		if err != nil {
			return err
		}
		submitted = true
		artifact, err := s.compactor.Compact(ctx, prior, turns, correctiveCode)
		if err != nil {
			if errors.Is(err, errInvalidCompactionOutput) && ctx.Err() == nil {
				mutex.Lock()
				recordErr := s.store.RecordCompressionCorrection(ctx, work, compactionErrorCode(err))
				mutex.Unlock()
				if recordErr != nil {
					s.log.Server("compaction").Warn("compaction.profile.correction_failed", "failed to persist compression correction", config.ErrorField(recordErr))
				}
			}
			return err
		}
		mutex.Lock()
		defer mutex.Unlock()
		if err := s.store.SaveCompressionArtifact(ctx, work, artifact); err != nil {
			return err
		}
		artifactSaved = true
		return s.store.PublishProfileSummary(ctx, work, turns, artifact)
	})
	if err != nil {
		status, outcome = "error", "failed"
		if workCtx.Err() != nil {
			status, outcome = "rejected", "canceled"
			refund = submitted && !artifactSaved
		} else if errors.Is(err, errPermanentProvider) || errors.Is(err, memory.ErrModelSubmissionBudgetExhausted) {
			dead = true
			status, outcome = "rejected", "skipped"
		}
	}
	// Cleanup must outlive foreground preemption, but is strictly bounded and
	// fenced by the last renewed token. It cannot release a reclaimed lease.
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	cleanupErr := s.store.ReleaseCompression(cleanup, work, refund, dead)
	cancel()
	if cleanupErr != nil && !errors.Is(cleanupErr, memory.ErrStaleSessionCompactionJobLease) && !errors.Is(cleanupErr, sql.ErrNoRows) {
		s.log.Server("compaction").Warn("compaction.profile.release_failed", "failed to release profile compression", config.ErrorField(cleanupErr))
	}
	s.log.Server("compaction").Info("compaction.profile.complete", "completed profile compression attempt", config.F("record_kind", "measurement"), config.F("user_id", scope.UserID), config.F("operation_id", meta.OperationID), config.F("status", status), config.F("outcome", outcome), config.F("turn_count", len(turns)), config.F("is_submitted", submitted), config.F("is_artifact_reused", artifactReused), config.F("duration_ms", time.Since(started).Milliseconds()), config.ErrorField(err))
	if err == nil {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}
