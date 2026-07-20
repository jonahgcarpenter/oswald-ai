package formationruntime

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
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

const (
	formationPollInterval = time.Second
	formationJobLease     = 2 * time.Minute
	formationMaxAttempts  = 5
)

// Service owns the durable post-turn formation worker.
type Service struct {
	store     *usermemory.Store
	extractor Extractor
	log       *config.Logger
	model     string
	notify    chan struct{}
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// NewService creates a serialized formation worker.
func NewService(store *usermemory.Store, extractor Extractor, model string, log *config.Logger) *Service {
	return &Service{store: store, extractor: extractor, model: model, log: log, notify: make(chan struct{}, 1)}
}

// Start begins startup recovery and polling.
func (s *Service) Start(parent context.Context) {
	if s == nil || s.store == nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.wg.Add(1)
	go s.run(ctx)
}

// Stop releases unfinished leases for restart and waits for the worker.
func (s *Service) Stop() {
	if s == nil || s.cancel == nil {
		return
	}
	s.cancel()
	s.wg.Wait()
}

// Enqueue records post-delivery extraction without running it inline.
func (s *Service) Enqueue(ctx context.Context, userID string, source usermemory.FormationSource) error {
	if err := s.store.MarkFormationEligible(ctx, userID, source.TurnID); err != nil {
		return err
	}
	if _, err := s.store.EnqueueFormationJob(ctx, source, userID); err != nil {
		return err
	}
	select {
	case s.notify <- struct{}{}:
	default:
	}
	return nil
}

func (s *Service) run(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(formationPollInterval)
	defer ticker.Stop()
	_, _ = s.store.ReconcileFormationJobs(ctx, s.model, usermemory.FormationExtractorVersion)
	s.publishApproved(ctx)
	ticks := 0
	for {
		s.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ticks++
			if ticks%60 == 0 {
				s.publishApproved(ctx)
				_, _ = s.store.RedriveDeadFormationJobs(ctx, 5*time.Minute)
				if _, err := s.store.ReconcileFormationJobs(ctx, s.model, usermemory.FormationExtractorVersion); err != nil {
					s.warn("memory.formation.job.reconcile_failed", "failed to reconcile memory formation jobs", err)
				}
			}
		case <-s.notify:
		}
	}
}

func (s *Service) publishApproved(ctx context.Context) {
	candidates, err := s.store.ApprovedUnpublishedCandidates(ctx, 20)
	if err != nil {
		s.warn("memory.formation.publication.scan_failed", "failed to scan approved memory candidates", err)
		return
	}
	for _, candidate := range candidates {
		if _, err := s.store.PublishCandidate(ctx, candidate.UserID, candidate.ID); err != nil {
			_ = s.store.DeferCandidatePublication(context.Background(), candidate.UserID, candidate.ID)
			s.warn("memory.formation.publication.retry", "approved memory publication will retry", err, config.F("candidate_id", candidate.ID), config.F("user_id", candidate.UserID), config.F("status", "retry"))
		}
	}
}

func (s *Service) drain(ctx context.Context) {
	for ctx.Err() == nil {
		job, err := s.store.ClaimFormationJob(ctx, formationJobLease)
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		if err != nil {
			s.warn("memory.formation.job.claim_failed", "failed to claim memory formation job", err)
			return
		}
		if err := s.process(ctx, job); err != nil {
			if errors.Is(err, errPermanentExtraction) {
				if skipErr := s.store.SkipFormationJob(context.Background(), job, errorCode(err)); skipErr != nil {
					s.warn("memory.formation.job.complete_failed", "failed to terminally skip memory formation job", skipErr, config.F("job_id", job.ID), config.F("user_id", job.UserID))
				} else {
					s.warn("memory.formation.job.skipped", "memory formation job returned invalid structured output", err, config.F("job_id", job.ID), config.F("user_id", job.UserID), config.F("attempt_count", job.AttemptCount))
				}
				continue
			}
			_ = s.store.RetryFormationJob(context.Background(), job, errorCode(err), formationMaxAttempts)
			state, _ := s.store.FormationJobState(context.Background(), job.UserID, job.ID)
			event, message, status := "memory.formation.job.retry", "memory formation job will retry", "retry"
			if state == "dead" {
				event, message, status = "memory.formation.job.dead", "memory formation job exhausted immediate retries", "degraded"
			}
			s.warn(event, message, err,
				config.F("job_id", job.ID), config.F("user_id", job.UserID), config.F("attempt_count", job.AttemptCount), config.F("job_state", state), config.F("status", status))
			continue
		}
		if err := s.store.CompleteFormationJob(context.Background(), job, false); err != nil {
			s.warn("memory.formation.job.complete_failed", "failed to complete memory formation job", err, config.F("job_id", job.ID))
		}
	}
}

func (s *Service) process(ctx context.Context, job usermemory.FormationJob) error {
	started := time.Now()
	turn, err := s.store.SessionTurnByID(ctx, job.UserID, job.TurnID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return s.store.CompleteFormationJob(context.Background(), job, true)
		}
		return err
	}
	explicitIDs, err := s.store.AttachRequestCandidates(ctx, job.UserID, job.RequestID, turn.ID)
	if err != nil {
		return err
	}
	for _, candidateID := range explicitIDs {
		if _, err := s.store.PublishCandidate(ctx, job.UserID, candidateID); err != nil {
			return err
		}
	}
	var extracted []ExtractedCandidate
	artifact, err := s.store.FormationJobArtifact(ctx, job)
	if err != nil {
		return err
	}
	if artifact != "" {
		if err := json.Unmarshal([]byte(artifact), &extracted); err != nil {
			return fmt.Errorf("decode persisted memory formation artifact: %w", err)
		}
	} else if s.extractor != nil {
		extractCtx := requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: fmt.Sprintf("%s:formation:%d", job.RequestID, job.ID), SessionID: job.SessionID, Model: job.Model, CurrentUserText: turn.UserText})
		extractCtx = requestctx.WithPrincipal(extractCtx, identity.Principal{CanonicalUserID: job.UserID, Gateway: "formation", ExternalID: job.UserID, Assurance: identity.AssuranceSelfAsserted})
		extracted, err = s.extractor.Extract(extractCtx, turn)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(extracted)
		if err != nil {
			return err
		}
		if err := s.store.SaveFormationJobArtifact(ctx, job, string(payload)); err != nil {
			return err
		}
	}
	createdCount := 0
	publishedCount := 0
	for _, raw := range extracted {
		output, err := evaluateExtracted(turn, raw)
		if err != nil {
			continue
		}
		candidate, created, err := s.store.ProposeCandidate(ctx, job.UserID, usermemory.CandidateProposal{
			Output:              output,
			Source:              usermemory.FormationSource{RequestID: job.RequestID, SessionID: turn.SessionID, SessionGeneration: turn.Generation, TurnID: turn.ID, Model: job.Model, ExtractorVersion: job.ExtractorVersion},
			IdempotencyKey:      extractedCandidateKey(turn.ID, job.ExtractorVersion, raw),
			SupersedesStatement: raw.SupersedesStatement,
			FormationJob:        &job,
		})
		if err != nil {
			return err
		}
		if created {
			createdCount++
		}
		if candidate.State == "approved" && candidate.PublishedMemoryID == 0 {
			if _, err := s.store.PublishCandidate(ctx, job.UserID, candidate.ID); err != nil {
				return err
			}
			publishedCount++
		}
	}
	if s.log != nil {
		s.log.Server("memory.formation").Info("memory.formation.extraction.complete", "completed memory formation extraction",
			config.F("job_id", job.ID), config.F("user_id", job.UserID), config.F("candidate_count", len(extracted)),
			config.F("created_count", createdCount), config.F("approved_count", publishedCount),
			config.F("duration_ms", time.Since(started).Milliseconds()), config.F("status", "ok"))
	}
	return nil
}

func extractedCandidateKey(turnID int64, version string, candidate ExtractedCandidate) string {
	payload, _ := json.Marshal(candidate)
	canonical := fmt.Sprintf("%d\x00%s\x00%s", turnID, version, payload)
	sum := sha256.Sum256([]byte(canonical))
	return "extract:" + hex.EncodeToString(sum[:])
}

func (s *Service) warn(event, message string, err error, fields ...config.Field) {
	if s.log == nil {
		return
	}
	fields = append(fields, config.F("status", "degraded"), config.ErrorField(err))
	s.log.Server("memory.formation").Warn(event, message, fields...)
}

func errorCode(err error) string {
	if err == nil {
		return "unknown"
	}
	if errors.Is(err, errPermanentExtraction) {
		var httpErr *llm.ChatHTTPError
		if errors.As(err, &httpErr) {
			return "provider_request_rejected"
		}
		return "invalid_output"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "transient_timeout"
	}
	var httpErr *llm.ChatHTTPError
	if errors.As(err, &httpErr) {
		if httpErr.StatusCode == 429 {
			return "transient_rate_limit"
		}
		return "transient_provider"
	}
	return "transient_runtime"
}
