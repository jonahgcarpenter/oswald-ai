package llm

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ChatHTTPError describes a non-success response from the chat completion endpoint.
type ChatHTTPError struct {
	StatusCode int
	// Body is bounded request-local provider text used only for narrow retry
	// classification. Error intentionally excludes it because providers may
	// reflect prompt content in error responses.
	Body string
}

func (e *ChatHTTPError) Error() string {
	return fmt.Sprintf("LLM gateway chat returned HTTP %d", e.StatusCode)
}

// HTTPStatusCode exposes the bounded provider status to telemetry.
func (e *ChatHTTPError) HTTPStatusCode() int { return e.StatusCode }

// IsPermanentChatProviderError reports request failures that should not be retried unchanged.
func IsPermanentChatProviderError(err error) bool {
	var httpErr *ChatHTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode >= http.StatusBadRequest && httpErr.StatusCode < http.StatusInternalServerError && httpErr.StatusCode != http.StatusRequestTimeout && httpErr.StatusCode != http.StatusTooEarly && httpErr.StatusCode != http.StatusTooManyRequests
}

// IsTemporaryOllamaToolParserError identifies a narrow upstream Ollama/Qwen
// tool-parser failure. Remove this workaround when the provider no longer
// turns malformed generated tool markup into HTTP 500 responses.
func IsTemporaryOllamaToolParserError(err error) bool {
	var httpErr *ChatHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusInternalServerError {
		return false
	}
	body := strings.ToLower(httpErr.Body)
	return strings.Contains(body, "expected element type") ||
		strings.Contains(body, "xml syntax error") ||
		(strings.Contains(body, "xml") && strings.Contains(body, "unexpected eof"))
}

// IsOllamaModelRunnerStoppedError identifies the upstream failure returned when
// Ollama stops a model runner while processing a request.
func IsOllamaModelRunnerStoppedError(err error) bool {
	var httpErr *ChatHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusInternalServerError {
		return false
	}
	return strings.Contains(strings.ToLower(httpErr.Body), "model runner has unexpectedly stopped")
}

// IsContextLengthExceededError identifies provider rejections caused by a
// request exceeding the active model context window.
func IsContextLengthExceededError(err error) bool {
	var httpErr *ChatHTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	body := strings.ToLower(httpErr.Body)
	return strings.Contains(body, "context size") ||
		strings.Contains(body, "context length") ||
		strings.Contains(body, "context window") ||
		strings.Contains(body, "too many tokens") ||
		strings.Contains(body, "model_max") ||
		strings.Contains(body, "out of context") ||
		strings.Contains(body, "prompt too long") ||
		strings.Contains(body, "maximum context") ||
		strings.Contains(body, "max_tokens")
}
