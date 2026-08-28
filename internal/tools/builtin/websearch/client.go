package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
)

const (
	maxResults             = 8
	maxCandidates          = 50
	maxResponseBytes       = 2 << 20
	maxQueryRunes          = 500
	maxURLBytes            = 2048
	maxTitleRunes          = 240
	maxSnippetRunes        = 800
	maxEngineNames         = 8
	maxEngineNameRunes     = 64
	maxUnresponsiveEngines = 8
	httpTimeout            = 8 * time.Second
	defaultRetryDelay      = 100 * time.Millisecond
	maxRetryDelay          = time.Second
)

var trackingParameters = map[string]struct{}{
	"fbclid": {}, "gclid": {}, "dclid": {}, "msclkid": {},
	"mc_cid": {}, "mc_eid": {}, "_ga": {}, "_gl": {},
}

// Client implements Searcher against a SearXNG instance.
type Client struct {
	searchURL  url.URL
	httpClient *http.Client
	log        *config.Logger
}

// NewClient creates a SearXNG web search client targeting an absolute HTTP(S)
// base URL. A path prefix is preserved when constructing the search endpoint.
func NewClient(baseURL string, log *config.Logger) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err == nil {
		parsed.Scheme = strings.ToLower(parsed.Scheme)
	}
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("invalid SearXNG base URL: absolute HTTP(S) URL with host required")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid SearXNG base URL: userinfo, query, and fragment are not allowed")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/search"
	parsed.RawPath = ""
	searchOrigin := parsed.Scheme + "://" + parsed.Host
	return &Client{
		searchURL: *parsed,
		httpClient: &http.Client{
			Timeout: httpTimeout,
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				if !strings.EqualFold(req.URL.Scheme+"://"+req.URL.Host, searchOrigin) {
					return http.ErrUseLastResponse
				}
				return nil
			},
		},
		log: log,
	}, nil
}

// Search queries SearXNG and returns only validated public web results.
func (c *Client) Search(ctx context.Context, query string) (SearchResponse, error) {
	startedAt := time.Now()
	if err := validateQuery(query); err != nil {
		return SearchResponse{}, err
	}
	query = strings.TrimSpace(query)

	requestURL := c.searchURL
	params := requestURL.Query()
	params.Set("q", query)
	params.Set("format", "json")
	params.Set("language", "en-US")
	params.Set("categories", "general")
	params.Set("pageno", "1")
	requestURL.RawQuery = params.Encode()

	var resp *http.Response
	for attempt := 1; attempt <= 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
		if err != nil {
			return SearchResponse{}, errors.New("failed to build SearXNG request")
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "oswald-ai/web.search")
		resp, err = c.httpClient.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return SearchResponse{}, fmt.Errorf("SearXNG request canceled: %w", ctxErr)
			}
			if attempt == 2 {
				c.logRequestFailure(ctx, 0, attempt, time.Since(startedAt))
				return SearchResponse{}, errors.New("SearXNG transport request failed")
			}
			c.logRetry(ctx, 0, attempt)
			if err := waitForRetry(ctx, defaultRetryDelay); err != nil {
				return SearchResponse{}, fmt.Errorf("SearXNG request canceled: %w", err)
			}
			continue
		}

		if resp.StatusCode == http.StatusOK {
			break
		}
		status := resp.StatusCode
		delay := retryDelay(resp.Header.Get("Retry-After"), time.Now())
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if attempt == 2 || !retryableStatus(status) {
			c.logRequestFailure(ctx, status, attempt, time.Since(startedAt))
			return SearchResponse{}, fmt.Errorf("SearXNG returned status %d", status)
		}
		c.logRetry(ctx, status, attempt)
		if err := waitForRetry(ctx, delay); err != nil {
			return SearchResponse{}, fmt.Errorf("SearXNG request canceled: %w", err)
		}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return SearchResponse{}, errors.New("failed to read SearXNG response")
	}
	if len(body) > maxResponseBytes {
		return SearchResponse{}, errors.New("SearXNG response exceeded size limit")
	}
	var backend searxngResponse
	if err := json.Unmarshal(body, &backend); err != nil {
		return SearchResponse{}, errors.New("failed to parse SearXNG response")
	}

	response := filterResponse(backend)
	c.logCompletion(ctx, query, len(body), response, time.Since(startedAt))
	return response, nil
}

func validateQuery(query string) error {
	if !utf8.ValidString(query) || utf8.RuneCountInString(query) > maxQueryRunes {
		return errors.New("search query exceeded 500 runes or was invalid UTF-8")
	}
	for _, r := range query {
		if unicode.IsControl(r) {
			return errors.New("search query contains disallowed control characters")
		}
	}
	if strings.TrimSpace(query) == "" {
		return errors.New("search query was empty")
	}
	return nil
}

func filterResponse(backend searxngResponse) SearchResponse {
	response := SearchResponse{
		Notice:              toolNotice,
		UnresponsiveEngines: unresponsiveEngineNames(backend.UnresponsiveEngines),
		Results:             make([]SearchResult, 0, maxResults),
		Stats: CandidateStats{
			CandidateCount: len(backend.Results),
		},
	}
	response.Degraded = len(response.UnresponsiveEngines) > 0
	seenURLs := make(map[string]int)
	hostCounts := make(map[string]int)
	inspect := min(len(backend.Results), maxCandidates)
	response.Stats.InspectedCount = inspect

	for _, candidate := range backend.Results[:inspect] {
		result, ok := normalizeResult(candidate)
		if !ok {
			response.Stats.FilteredCount++
			continue
		}
		if index, duplicate := seenURLs[result.URL]; duplicate {
			response.Results[index].Engines = mergeNames(response.Results[index].Engines, result.Engines, maxEngineNames)
			response.Stats.DuplicateCount++
			continue
		}
		if hostCounts[result.Domain] >= 2 {
			response.Stats.FilteredCount++
			continue
		}
		if len(response.Results) >= maxResults {
			continue
		}
		seenURLs[result.URL] = len(response.Results)
		hostCounts[result.Domain]++
		response.Results = append(response.Results, result)
	}
	return response
}

func normalizeResult(candidate searxngResult) (SearchResult, bool) {
	title := truncateRunes(cleanText(candidate.Title), maxTitleRunes)
	if title == "" || len(candidate.URL) > maxURLBytes {
		return SearchResult{}, false
	}
	parsed, err := url.Parse(candidate.URL)
	if err == nil {
		parsed.Scheme = strings.ToLower(parsed.Scheme)
	}
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return SearchResult{}, false
	}
	domain := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if domain == "" || rejectedLiteralIP(domain) {
		return SearchResult{}, false
	}
	parsed.Host = canonicalHost(parsed, domain)
	parsed.Fragment = ""
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return SearchResult{}, false
	}
	for key := range query {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "utm_") {
			query.Del(key)
			continue
		}
		if _, tracked := trackingParameters[lower]; tracked {
			query.Del(key)
		}
	}
	parsed.RawQuery = query.Encode()
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	canonical := parsed.String()
	if len(canonical) > maxURLBytes {
		return SearchResult{}, false
	}

	engines := mergeNames(nil, append([]string{candidate.Engine}, candidate.Engines...), maxEngineNames)
	snippet := truncateRunes(cleanText(candidate.Content), maxSnippetRunes)
	return SearchResult{
		Title:       title,
		URL:         canonical,
		Domain:      domain,
		Snippet:     snippet,
		Engines:     engines,
		PublishedAt: truncateRunes(cleanText(candidate.PublishedDate), 64),
		Score:       candidate.Score,
		Category:    truncateRunes(cleanText(candidate.Category), 64),
		Positions:   append([]int(nil), candidate.Positions...),
	}, true
}

func canonicalHost(parsed *url.URL, domain string) string {
	port := parsed.Port()
	if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(domain, ":") {
		domain = "[" + domain + "]"
	}
	if port != "" {
		return domain + ":" + port
	}
	return domain
}

func rejectedLiteralIP(host string) bool {
	address, err := netip.ParseAddr(host)
	if err != nil {
		return strings.EqualFold(host, "localhost")
	}
	return address.IsUnspecified() || address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast()
}

func cleanText(value string) string {
	value = html.UnescapeString(value)
	var builder strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) {
			builder.WriteRune(' ')
			continue
		}
		builder.WriteRune(r)
	}
	return strings.Join(strings.Fields(builder.String()), " ")
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}

func mergeNames(existing, incoming []string, limit int) []string {
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	out := make([]string, 0, min(limit, len(existing)+len(incoming)))
	for _, raw := range append(append([]string(nil), existing...), incoming...) {
		name := truncateRunes(cleanText(raw), maxEngineNameRunes)
		key := strings.ToLower(name)
		if name == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, name)
		if len(out) == limit {
			break
		}
	}
	return out
}

func unresponsiveEngineNames(values [][]interface{}) []string {
	names := make([]string, 0, min(len(values), maxUnresponsiveEngines))
	for _, entry := range values {
		if len(entry) == 0 {
			continue
		}
		name, ok := entry[0].(string)
		if !ok {
			continue
		}
		names = mergeNames(names, []string{name}, maxUnresponsiveEngines)
	}
	return names
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func retryDelay(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		if seconds >= int64(maxRetryDelay/time.Second) {
			return maxRetryDelay
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return min(max(when.Sub(now), time.Duration(0)), maxRetryDelay)
	}
	return defaultRetryDelay
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) logRetry(ctx context.Context, status, attempt int) {
	if c.log == nil {
		return
	}
	fields := requestLogFields(ctx, config.F("attempt", attempt), config.F("status", "retry"))
	if status != 0 {
		fields = append(fields, config.F("http_status", status))
	}
	c.log.Warn("tool.web.search.retry", "retrying web search request", fields...)
}

func (c *Client) logRequestFailure(ctx context.Context, status, attempt int, duration time.Duration) {
	if c.log == nil {
		return
	}
	fields := requestLogFields(ctx,
		config.F("attempt_count", attempt),
		config.F("duration_ms", duration.Milliseconds()),
		config.F("status", "error"),
	)
	if status != 0 {
		fields = append(fields, config.F("http_status", status))
	}
	c.log.Warn("tool.web.search.request_failed", "web search request failed", fields...)
}

func (c *Client) logCompletion(ctx context.Context, query string, responseBytes int, response SearchResponse, duration time.Duration) {
	if c.log == nil {
		return
	}
	status := "ok"
	if response.Degraded {
		status = "degraded"
	}
	fields := requestLogFields(ctx,
		config.F("query_chars", utf8.RuneCountInString(query)),
		config.F("response_bytes", responseBytes),
		config.F("candidate_count", response.Stats.CandidateCount),
		config.F("inspected_count", response.Stats.InspectedCount),
		config.F("filtered_count", response.Stats.FilteredCount),
		config.F("duplicate_count", response.Stats.DuplicateCount),
		config.F("result_count", len(response.Results)),
		config.F("unresponsive_engine_count", len(response.UnresponsiveEngines)),
		config.F("duration_ms", duration.Milliseconds()),
		config.F("is_degraded", response.Degraded),
		config.F("status", status),
	)
	if response.Degraded {
		c.log.Warn("tool.web.search.results_degraded", "web search returned partial results", fields...)
		return
	}
	c.log.Debug("tool.web.search.results_returned", "web search returned results", fields...)
}

func requestLogFields(ctx context.Context, fields ...config.Field) []config.Field {
	meta := requestctx.MetadataFromContext(ctx)
	return append([]config.Field{config.F("request_id", meta.RequestID)}, fields...)
}
