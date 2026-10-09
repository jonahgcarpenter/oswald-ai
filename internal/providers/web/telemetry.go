package web

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

// SearchHTTPError retains the status without logging the response body.
type SearchHTTPError struct {
	status  int
	message string
}

func (e *SearchHTTPError) Error() string       { return e.message }
func (e *SearchHTTPError) HTTPStatusCode() int { return e.status }

// NewSearchHTTPError constructs a status-only provider error.
func NewSearchHTTPError(status int, message string) error {
	return &SearchHTTPError{status: status, message: message}
}

// BeginSearch creates a child operation and its terminal provider measurement.
func BeginSearch(ctx context.Context, log *config.Logger, provider string) (context.Context, func(SearchResponse, error)) {
	started := time.Now()
	meta := requestctx.MetadataFromContext(ctx)
	parent := meta.OperationID
	if parent == "" {
		parent = meta.ParentOperationID
	}
	meta.OperationID, meta.ParentOperationID = rand.Text(), parent
	ctx = requestctx.WithMetadata(ctx, meta)
	return ctx, func(response SearchResponse, err error) {
		if log == nil {
			return
		}
		status, outcome := "ok", "ok"
		if response.Degraded {
			status, outcome = "degraded", "degraded"
		}
		if err != nil {
			status, outcome = "error", "error"
			if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				status, outcome = "ok", "canceled"
				err = context.Canceled
			}
		}
		fields := RequestLogFields(ctx, config.F("record_kind", "measurement"), config.F("provider", provider), config.F("operation", "search"), config.F("status", status), config.F("outcome", outcome), config.F("duration_ms", time.Since(started).Milliseconds()), config.F("result_count", len(response.Results)), config.F("candidate_count", response.Stats.CandidateCount), config.F("filtered_count", response.Stats.FilteredCount))
		if err != nil {
			fields = append(fields, config.ErrorField(err))
		}
		if code := config.HTTPStatus(err); code != 0 {
			fields = append(fields, config.F("http_status", code))
		}
		event := "provider.web.search.complete"
		if provider == "fallback" {
			event = "provider.web.search.fallback.complete"
		}
		log.Server("provider.web.search").Debug(event, "web search completed", fields...)
		if status == "degraded" || status == "error" {
			log.Server("provider.web.search").Warn("provider.web.search.degraded", "web search unavailable or degraded", RequestLogFields(ctx, config.F("provider", provider), config.F("status", status), config.ErrorField(err))...)
		}
	}
}

// Attempt timing stops at response headers; the enclosing search includes body
// decoding and retries. Neither transport errors nor request URLs are logged.
// SearchAttempt records a bounded HTTP-header attempt measurement.
func SearchAttempt(log *config.Logger, client *http.Client, req *http.Request, provider string, attempt int) (*http.Response, error) {
	started := time.Now()
	resp, err := client.Do(req)
	if log == nil {
		return resp, err
	}
	status, outcome := "ok", "ok"
	code := 0
	if resp != nil {
		code = resp.StatusCode
	}
	if err != nil || code < 200 || code >= 300 {
		status, outcome = "error", "error"
	}
	if errors.Is(err, context.Canceled) {
		status, outcome = "ok", "canceled"
	}
	fields := RequestLogFields(req.Context(), config.F("record_kind", "measurement"), config.F("provider", provider), config.F("operation", "search"), config.F("phase", "headers"), config.F("attempt_count", attempt), config.F("status", status), config.F("outcome", outcome), config.F("duration_ms", time.Since(started).Milliseconds()))
	if code != 0 {
		fields = append(fields, config.F("http_status", code))
	}
	if err != nil {
		fields = append(fields, config.ErrorField(err))
	}
	log.Server("provider.web.search").Debug("provider.web.search.attempt.complete", "web search HTTP attempt completed", fields...)
	return resp, err
}

// RequestLogFields adds available safe request correlation fields.
func RequestLogFields(ctx context.Context, fields ...config.Field) []config.Field {
	return append(requestctx.LogFields(ctx), fields...)
}
