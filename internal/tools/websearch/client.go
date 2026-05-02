package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

const (
	// maxResults caps the number of results returned per query to keep context manageable.
	maxResults = 5

	// httpTimeout is the per-request timeout for SearXNG calls.
	httpTimeout = 10 * time.Second
)

// Client implements Searcher against a local SearXNG instance.
type Client struct {
	baseURL    string
	httpClient *http.Client
	log        *config.Logger
}

// NewClient creates a SearXNG web search client targeting the given base URL.
func NewClient(baseURL string, log *config.Logger) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: httpTimeout,
		},
		log: log,
	}
}

// Search queries the configured SearXNG instance for the given query string.
func (c *Client) Search(ctx context.Context, query string) ([]SearchResult, error) {
	reqURL := fmt.Sprintf("%s/search?q=%s&format=json", c.baseURL, url.QueryEscape(query))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build SearXNG request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.log.Warn("tool.web_search.request_failed", "web search request failed",
			config.F("query_chars", len(query)),
			config.F("status", "error"),
			config.ErrorField(err),
		)
		return nil, fmt.Errorf("SearXNG request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		c.log.Warn("tool.web_search.response_failed", "web search response failed",
			config.F("query_chars", len(query)),
			config.F("http_status", resp.StatusCode),
			config.F("status", "error"),
			config.F("body_preview", string(body)),
		)
		return nil, fmt.Errorf("SearXNG returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read SearXNG response: %w", err)
	}

	var sr searxngResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("failed to parse SearXNG response: %w", err)
	}

	results := sr.Results
	if len(results) > maxResults {
		results = results[:maxResults]
	}

	out := make([]SearchResult, len(results))
	for i, r := range results {
		out[i] = SearchResult{
			Title:   r.Title,
			URL:     r.URL,
			Content: r.Content,
		}
	}

	c.log.Debug("tool.web_search.results_returned", "web search returned results", config.F("query_chars", len(query)), config.F("result_count", len(out)))
	return out, nil
}
