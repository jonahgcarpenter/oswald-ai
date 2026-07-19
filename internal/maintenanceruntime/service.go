// Package maintenanceruntime runs serialized periodic retention and database
// consistency sweeps.
package maintenanceruntime

import (
	"context"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

// Sweeper performs one complete maintenance pass.
type Sweeper interface {
	MaintenanceSweep(context.Context, time.Time, config.RetentionPolicy) (usermemory.MaintenanceCounts, error)
}

// Service owns the single periodic maintenance goroutine.
type Service struct {
	sweeper Sweeper
	policy  config.RetentionPolicy
	log     *config.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewService creates a periodic maintenance service.
func NewService(sweeper Sweeper, policy config.RetentionPolicy, log *config.Logger) *Service {
	if policy.MaintenanceInterval <= 0 {
		policy.MaintenanceInterval = time.Hour
	}
	if log != nil {
		log = log.Server("maintenanceruntime")
	}
	return &Service{sweeper: sweeper, policy: policy, log: log}
}

// Start launches an immediate sweep followed by interval-based sweeps.
func (s *Service) Start(parent context.Context) {
	if s == nil || s.sweeper == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.wg.Add(1)
	go s.run(ctx)
}

// Stop cancels the worker and waits for the active sweep to return.
func (s *Service) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

func (s *Service) run(ctx context.Context) {
	defer s.wg.Done()
	s.sweep(ctx)
	ticker := time.NewTicker(s.policy.MaintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.sweepAt(ctx, now.UTC())
		}
	}
}

func (s *Service) sweep(ctx context.Context) {
	s.sweepAt(ctx, time.Now().UTC())
}

func (s *Service) sweepAt(ctx context.Context, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	started := time.Now()
	counts, err := s.sweeper.MaintenanceSweep(ctx, now, s.policy)
	if err != nil {
		if s.log != nil && ctx.Err() == nil {
			s.log.Warn("maintenance.sweep.failed", "periodic maintenance sweep failed", config.F("rows_changed", counts.Changed()), config.F("duration_ms", time.Since(started).Milliseconds()), config.F("status", "degraded"), config.ErrorField(err))
		}
		return
	}
	if s.log != nil {
		s.log.Info("maintenance.sweep.complete", "periodic maintenance sweep completed",
			config.F("rows_changed", counts.Changed()),
			config.F("forgotten_memory_count", counts.ForgottenMemories),
			config.F("redacted_count", counts.AuditRowsRedacted+counts.FormationJobsRedacted+counts.CompactionJobsRedacted+counts.CandidatesRedacted+counts.EvidenceRowsRedacted+counts.EventsRedacted),
			config.F("tombstone_deleted_count", counts.AuditTombstones+counts.MemoryTombstonesDeleted+counts.CandidateTombstones+counts.EventTombstones+counts.PrivacyTombstones+counts.InvalidationTombstones+counts.FormationJobsDeleted+counts.CompactionJobsDeleted),
			config.F("challenge_deleted_count", counts.ChallengesDeleted),
			config.F("index_row_deleted_count", counts.IndexRowsDeleted),
			config.F("index_revision_degraded_count", counts.IndexRevisionsDegraded),
			config.F("index_table_dropped_count", counts.IndexTablesDropped),
			config.F("duration_ms", time.Since(started).Milliseconds()),
			config.F("status", "ok"),
		)
	}
}
