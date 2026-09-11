package documents

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

// Service sequentially extracts durable document jobs without a model permit.
type Service struct {
	store   *memory.Store
	log     *config.Logger
	extract func(context.Context, string, string, []byte) (Result, error)
	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewService creates a worker backed by the local, deadline-bounded extractor.
func NewService(store *memory.Store, log *config.Logger) *Service {
	return &Service{store: store, log: log, extract: NewExtractor(log).Extract}
}

// Start claims queued and abandoned jobs immediately, then polls every two seconds.
func (s *Service) Start(parent context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if s.log != nil {
			s.log.Server("documents").Info("documents.worker.started", "document extraction worker started", config.F("workload", "document_extraction"))
			defer s.log.Server("documents").Info("documents.worker.stopped", "document extraction worker stopped", config.F("workload", "document_extraction"))
		}
		owner := rand.Text()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for ctx.Err() == nil {
			claimCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			job, err := s.store.ClaimUserDocumentExtraction(claimCtx, owner)
			cancel()
			if err != nil && ctx.Err() == nil && s.log != nil {
				s.log.Server("documents").Warn("documents.worker.claim_failed", "document job claim failed", config.F("workload", "document_extraction"), config.ErrorField(err))
			}
			if err == nil && job != nil {
				s.process(ctx, job)
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Stop cancels extraction and joins it, including subprocess cleanup, before stores close.
// The service is single-start; repeated Stop calls are safe.
func (s *Service) Stop() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Service) process(ctx context.Context, job *memory.DocumentExtractionJob) {
	started := time.Now()
	operationID := rand.Text()
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{OperationID: operationID, Workload: "document_extraction"})
	status, outcome, code := "ok", "canceled", ""
	var committed int
	var storedBytes int
	defer func() {
		if s.log != nil {
			s.log.Server("documents").Info("documents.job.complete", "document extraction attempt completed",
				config.F("record_kind", "measurement"), config.F("workload", "document_extraction"),
				config.F("operation_id", operationID), config.F("user_id", job.UserID),
				config.F("status", status), config.F("outcome", outcome), config.F("reason_code", code),
				config.F("chunk_count", committed), config.F("text_bytes", storedBytes),
				config.F("duration_ms", time.Since(started).Milliseconds()))
		}
	}()
	if ctx.Err() != nil {
		return
	}
	// Three minutes of extraction leaves two minutes on the five-minute lease.
	// Renew synchronously before publication; no heartbeat can race token rotation.
	extractCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	result, err := s.extract(extractCtx, job.Document.Filename, job.Document.MediaType, job.Data)
	extractionErr := extractCtx.Err()
	cancel()
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	if extractionErr != nil {
		err = extractionErr
	}
	chunks, partial := storageChunks(result)
	if err != nil && !(errors.Is(err, ErrLimit) && len(chunks) > 0) {
		code = "extraction_failed"
		if errors.Is(err, ErrUnsupported) {
			code = "unsupported_format"
		}
		if errors.Is(err, context.DeadlineExceeded) {
			code = "extraction_timeout"
		}
	} else if len(chunks) == 0 {
		code = "no_extractable_text"
	}
	if err != nil {
		partial = true
	}
	if ctx.Err() != nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	renewed, writeErr := s.store.RenewUserDocumentExtraction(writeCtx, job)
	if writeErr == nil && writeCtx.Err() == nil {
		if code != "" {
			writeErr = s.store.FailUserDocumentExtraction(writeCtx, renewed, code)
		} else {
			writeErr = s.store.CompleteUserDocumentExtraction(writeCtx, renewed, chunks, partial)
		}
	} else if writeErr == nil {
		writeErr = writeCtx.Err()
	}
	if writeErr != nil {
		if ctx.Err() != nil {
			code = ""
			return
		}
		status, outcome, code = "degraded", "reclaimable", "storage_failed"
		if errors.Is(writeErr, memory.ErrDocumentLease) {
			outcome, code = "stale", "lease_lost"
		}
		return
	}
	if code != "" {
		status, outcome = "error", "failed"
		return
	}
	outcome = "ready"
	if partial {
		status, outcome = "degraded", "partial"
	}
	committed = len(chunks)
	for _, c := range chunks {
		storedBytes += len(c.Text) + len(c.Locator) + len(c.Method)
	}
}

// storageChunks converts one-based extractor records to contiguous zero-based
// storage ordinals, including any splits. Metadata consumes the same byte budget.
func storageChunks(result Result) ([]memory.DocumentChunk, bool) {
	var chunks []memory.DocumentChunk
	partial, remaining := result.Partial, memory.DocumentMaxTextBytes
	for _, c := range result.Chunks {
		text := c.Text
		for len(text) > 0 {
			if strings.TrimSpace(text) == "" {
				break
			}
			n := min(len(text), memory.DocumentMaxChunkBytes, remaining-len(c.Locator)-len(c.Method))
			if n <= 0 || len(chunks) == memory.DocumentMaxChunks {
				return chunks, true
			}
			for n > 0 && !utf8.ValidString(text[:n]) {
				n--
			}
			if n == 0 {
				return chunks, true
			}
			if strings.TrimSpace(text[:n]) != "" {
				chunks = append(chunks, memory.DocumentChunk{Ordinal: len(chunks), Text: text[:n], Locator: c.Locator, Method: c.Method})
				remaining -= n + len(c.Locator) + len(c.Method)
			}
			text = text[n:]
		}
	}
	return chunks, partial
}
