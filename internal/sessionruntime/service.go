package sessionruntime

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

const (
	minimumRecentTail       = 8
	maximumCompactionRange  = 64
	compactionCountTrigger  = 24
	compactionBudgetPercent = 45
)

var errBackgroundPreempted = errors.New("background model work preempted by foreground traffic")

// LowPriorityGate grants model capacity only while foreground work is idle.
type LowPriorityGate interface {
	TryAcquireLowPriority(context.Context) (context.Context, func(), bool)
}

// Service plans and serially executes durable session compaction jobs.
type Service struct {
	store        *usermemory.Store
	extractor    Extractor
	model        string
	budget       promptbudget.ContextBudget
	log          *config.Logger
	owner        string
	lease        time.Duration
	gate         LowPriorityGate
	planRequests chan usermemory.ActiveSessionScope
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

// SetLowPriorityGate makes compaction model calls yield to foreground work.
func (s *Service) SetLowPriorityGate(gate LowPriorityGate) {
	s.gate = gate
}

// NewService constructs the post-delivery compaction service.
func NewService(store *usermemory.Store, extractor Extractor, model string, budget promptbudget.ContextBudget, requestTimeout time.Duration, log *config.Logger) *Service {
	lease := requestTimeout + 30*time.Second
	if lease < 2*time.Minute {
		lease = 2 * time.Minute
	}
	return &Service{store: store, extractor: extractor, model: model, budget: budget, log: log, owner: fmt.Sprintf("session-compactor-%d", time.Now().UnixNano()), lease: lease, planRequests: make(chan usermemory.ActiveSessionScope, 128)}
}

// Start reconciles durable state and starts one serialized worker.
func (s *Service) Start(parent context.Context) {
	if s == nil || s.store == nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.wg.Add(1)
	go s.run(ctx)
}

// Stop cancels work and waits for the worker; leases remain recoverable.
func (s *Service) Stop() {
	if s == nil || s.cancel == nil {
		return
	}
	s.cancel()
	s.wg.Wait()
}

// Enqueue marks delivery and proactively plans a fixed compaction range.
func (s *Service) Enqueue(ctx context.Context, userID string, source usermemory.FormationSource) error {
	if source.TurnID <= 0 || source.SessionGeneration <= 0 {
		return fmt.Errorf("session compaction enqueue requires source turn and generation")
	}
	if err := s.store.MarkSessionTurnDelivered(ctx, userID, source.TurnID); err != nil {
		return err
	}
	select {
	case s.planRequests <- usermemory.ActiveSessionScope{UserID: userID, SessionID: source.SessionID, Generation: source.SessionGeneration}:
	default:
	}
	return nil
}

// MarkDeliveryFailed records a terminal send failure without exposing the turn.
func (s *Service) MarkDeliveryFailed(ctx context.Context, userID string, turnID int64) error {
	return s.store.MarkSessionTurnDeliveryFailed(ctx, userID, turnID)
}

func (s *Service) run(ctx context.Context) {
	defer s.wg.Done()
	_, _ = s.store.ReconcileSessionCompactionJobs(ctx)
	s.planActiveSessions(ctx)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	ticks := 0
	for {
		s.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case scope := <-s.planRequests:
			if _, err := s.plan(ctx, scope.UserID, scope.SessionID, scope.Generation); err != nil {
				s.warn("session.compaction.plan.failed", "failed to plan session compaction", err, config.F("user_id", scope.UserID), config.F("session_id", scope.SessionID))
			}
		case <-ticker.C:
			ticks++
			if ticks%60 == 0 {
				_, _ = s.store.ReconcileSessionCompactionJobs(ctx)
				_, _ = s.store.RedriveDeadSessionCompactionJobs(ctx, 5*time.Minute)
				s.planActiveSessions(ctx)
			}
		}
	}
}

func (s *Service) planActiveSessions(ctx context.Context) {
	scopes, err := s.store.ActiveSessionScopes(ctx, 0)
	if err != nil {
		s.warn("session.compaction.plan.scan_failed", "failed to scan active sessions", err)
		return
	}
	for _, scope := range scopes {
		if _, err := s.plan(ctx, scope.UserID, scope.SessionID, scope.Generation); err != nil {
			s.warn("session.compaction.plan.failed", "failed to plan session compaction", err, config.F("user_id", scope.UserID), config.F("session_id", scope.SessionID))
		}
	}
}

func (s *Service) plan(ctx context.Context, userID, sessionID string, generation int) (int64, error) {
	var latest usermemory.SessionSummary
	boundary := int64(0)
	latest, err := s.store.LatestSessionSummary(ctx, userID, sessionID, generation)
	if err == nil {
		boundary = latest.CoveredThroughTurnID
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	available, err := s.store.DeliveredSessionTurnsAfter(ctx, userID, sessionID, generation, boundary, 1000)
	if err != nil {
		return 0, err
	}
	if len(available.Turns) <= minimumRecentTail {
		return 0, nil
	}
	estimate := estimateTurns(available.Turns)
	threshold := s.budget.UsableInputLimit() * compactionBudgetPercent / 100
	if threshold < 1024 {
		threshold = 1024
	}
	if available.TotalCount <= compactionCountTrigger && estimate <= threshold {
		return 0, nil
	}
	coverCount := len(available.Turns) - minimumRecentTail
	if coverCount > maximumCompactionRange {
		coverCount = maximumCompactionRange
	}
	if coverCount <= 0 {
		return 0, nil
	}
	var previous *usermemory.SessionSummary
	if latest.ID > 0 {
		previous = &latest
	}
	inputLimit := s.budget.UsableInputLimit()
	for coverCount > 0 {
		messages, buildErr := compactionMessages(previous, available.Turns[:coverCount])
		if buildErr != nil {
			return 0, buildErr
		}
		if promptbudget.EstimateRequest(messages, nil) <= inputLimit {
			break
		}
		coverCount--
	}
	if coverCount == 0 {
		return 0, fmt.Errorf("oldest complete exchange cannot fit the session compaction input budget")
	}
	from := available.Turns[0].ID
	if latest.ID > 0 {
		from = latest.CoveredFromTurnID
	}
	through := available.Turns[coverCount-1].ID
	return s.store.EnqueueSessionCompactionJob(ctx, userID, sessionID, generation, from, through)
}

func estimateTurns(turns []usermemory.SessionTurn) int {
	messages := make([]llm.ChatMessage, 0, len(turns)*2)
	for _, turn := range turns {
		messages = append(messages, llm.ChatMessage{Role: "user", Content: turn.UserText}, llm.ChatMessage{Role: "assistant", Content: turn.AssistantText})
	}
	return promptbudget.EstimateRequest(messages, nil)
}

func (s *Service) drain(ctx context.Context) {
	for ctx.Err() == nil {
		job, err := s.store.ClaimSessionCompactionJob(ctx, s.owner, s.lease)
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		if err != nil {
			s.warn("session.compaction.job.claim_failed", "failed to claim session compaction job", err)
			return
		}
		if err := s.process(ctx, job); err != nil {
			if errors.Is(err, errBackgroundPreempted) {
				if deferErr := s.store.DeferSessionCompactionJob(context.Background(), job, time.Second); deferErr != nil {
					s.warn("session.compaction.job.defer_failed", "failed to defer preempted session compaction job", deferErr, config.F("job_id", job.ID))
				}
				return
			}
			_ = s.store.RetrySessionCompactionJob(context.Background(), job, fmt.Sprintf("%T", err), err.Error())
			s.warn("session.compaction.job.retry", "session compaction job will retry", err, config.F("job_id", job.ID), config.F("attempt_count", job.AttemptCount), config.F("status", "retry"))
			continue
		}
		_, _ = s.plan(ctx, job.UserID, job.SessionID, job.SessionGeneration)
	}
}

func (s *Service) process(ctx context.Context, job usermemory.SessionCompactionJob) error {
	artifact, err := s.store.SessionCompactionArtifact(ctx, job)
	if errors.Is(err, sql.ErrNoRows) {
		artifact, err = s.generateArtifact(ctx, job)
		if err != nil {
			return err
		}
		if err := s.validateCandidates(ctx, job, artifact); err != nil {
			return err
		}
		if err := s.store.SaveSessionCompactionArtifact(ctx, job, artifact); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := s.stageCandidates(ctx, job, artifact); err != nil {
		return err
	}
	summary, err := s.store.PublishSessionSummary(ctx, job)
	if err != nil {
		return err
	}
	if err := s.store.CompleteSessionCompactionJob(ctx, job, false); err != nil {
		return err
	}
	if s.log != nil {
		s.log.Server("session.compaction").Info("session.compaction.complete", "completed session compaction", config.F("job_id", job.ID), config.F("user_id", job.UserID), config.F("session_id", job.SessionID), config.F("covered_turn_count", len(summary.SourceTurnIDs)), config.F("candidate_count", len(artifact.Candidates)), config.F("status", "ok"))
	}
	return nil
}

func (s *Service) generateArtifact(ctx context.Context, job usermemory.SessionCompactionJob) (usermemory.SummaryArtifact, error) {
	previous, turns, err := s.newTurnsForJob(ctx, job)
	if err != nil {
		return usermemory.SummaryArtifact{}, err
	}
	extractParent := ctx
	release := func() {}
	if s.gate != nil {
		var acquired bool
		extractParent, release, acquired = s.gate.TryAcquireLowPriority(ctx)
		if !acquired {
			return usermemory.SummaryArtifact{}, errBackgroundPreempted
		}
	}
	defer release()
	extractCtx := requestctx.WithMetadata(extractParent, requestctx.Metadata{RequestID: fmt.Sprintf("session-compaction:%d", job.ID), SessionID: job.SessionID, SessionGeneration: job.SessionGeneration, Model: s.model})
	extractCtx = requestctx.WithPrincipal(extractCtx, identity.Principal{CanonicalUserID: job.UserID, Gateway: "session_compaction", ExternalID: job.UserID, Assurance: identity.AssuranceSelfAsserted})
	artifact, err := s.extractor.Compact(extractCtx, previous, turns)
	wasPreempted := extractParent.Err() != nil && ctx.Err() == nil
	release()
	release = func() {}
	if wasPreempted {
		return usermemory.SummaryArtifact{}, errBackgroundPreempted
	}
	if err != nil {
		return usermemory.SummaryArtifact{}, err
	}
	artifact.GenerationModel = s.model
	artifact.GeneratorVersion = SummaryGeneratorVersion
	return artifact, nil
}

func (s *Service) newTurnsForJob(ctx context.Context, job usermemory.SessionCompactionJob) (*usermemory.SessionSummary, []usermemory.SessionTurn, error) {
	var previous *usermemory.SessionSummary
	boundary := job.CoveredFromTurnID - 1
	prior, err := s.store.SessionSummaryBefore(ctx, job.UserID, job.SessionID, job.SessionGeneration, job.CoveredThroughTurnID)
	if err == nil {
		previous = &prior
		boundary = prior.CoveredThroughTurnID
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	turns, err := s.store.DeliveredSessionTurnsRange(ctx, job.UserID, job.SessionID, job.SessionGeneration, boundary+1, job.CoveredThroughTurnID)
	if err != nil {
		return nil, nil, err
	}
	if len(turns) == 0 {
		return nil, nil, fmt.Errorf("session compaction range has no new delivered turns")
	}
	return previous, turns, nil
}

func (s *Service) validateCandidates(ctx context.Context, job usermemory.SessionCompactionJob, artifact usermemory.SummaryArtifact) error {
	_, turns, err := s.newTurnsForJob(ctx, job)
	if err != nil {
		return err
	}
	_, err = evaluateCompactionCandidates(turns, artifact.Candidates)
	return err
}

func (s *Service) stageCandidates(ctx context.Context, job usermemory.SessionCompactionJob, artifact usermemory.SummaryArtifact) error {
	if len(artifact.Candidates) == 0 {
		return nil
	}
	_, turns, err := s.newTurnsForJob(ctx, job)
	if err != nil {
		return err
	}
	evaluated, err := evaluateCompactionCandidates(turns, artifact.Candidates)
	if err != nil {
		return err
	}
	for i, item := range evaluated {
		raw, turn, output := artifact.Candidates[i], item.turn, item.output
		encoded, _ := json.Marshal(raw)
		sum := sha256.Sum256(append([]byte(fmt.Sprintf("%d:%d:%d:", job.ID, job.CoveredFromTurnID, job.CoveredThroughTurnID)), encoded...))
		_, _, err = s.store.ProposeCandidate(ctx, job.UserID, usermemory.CandidateProposal{
			Output: output, IdempotencyKey: "compact:" + hex.EncodeToString(sum[:]),
			Source:        usermemory.FormationSource{RequestID: fmt.Sprintf("session-compaction:%d", job.ID), SessionID: job.SessionID, SessionGeneration: job.SessionGeneration, TurnID: turn.ID, Model: s.model, ExtractorVersion: SummaryGeneratorVersion},
			CompactionJob: &job,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

type evaluatedCompactionCandidate struct {
	turn   usermemory.SessionTurn
	output memoryformation.CandidateOutput
}

func evaluateCompactionCandidates(turns []usermemory.SessionTurn, candidates []usermemory.CompactionCandidateArtifact) ([]evaluatedCompactionCandidate, error) {
	byID := make(map[int64]usermemory.SessionTurn, len(turns))
	for _, turn := range turns {
		byID[turn.ID] = turn
	}
	result := make([]evaluatedCompactionCandidate, 0, len(candidates))
	for _, raw := range candidates {
		turn, ok := byID[raw.SourceTurnID]
		if !ok {
			return nil, fmt.Errorf("compaction candidate source turn %d is outside newly covered range", raw.SourceTurnID)
		}
		output, err := memoryformation.Evaluate(memoryformation.CandidateInput{SourceUserText: turn.UserText, Statement: raw.Statement, Evidence: raw.Evidence, Provenance: memoryformation.Provenance(raw.Provenance), ClaimedAuthority: memoryformation.AuthorityModel, Sensitivity: memoryformation.Sensitivity(raw.Sensitivity), Mode: memoryformation.ModePreCompactionExtraction, Scope: memoryformation.Scope(raw.Scope), Category: memoryformation.Category(raw.Category), Context: memoryformation.ContentContext(raw.Context), Confidence: raw.Confidence, Importance: raw.Importance, TTL: time.Duration(raw.TTLDays) * 24 * time.Hour})
		if err != nil {
			return nil, err
		}
		result = append(result, evaluatedCompactionCandidate{turn: turn, output: output})
	}
	return result, nil
}

func (s *Service) warn(event, message string, err error, fields ...config.Field) {
	if s.log == nil {
		return
	}
	fields = append(fields, config.F("status", "degraded"), config.ErrorField(err))
	s.log.Server("session.compaction").Warn(event, message, fields...)
}
