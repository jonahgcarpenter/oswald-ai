package websearch

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/toolctx"
)

// SearchResult holds a single result returned by a web search.
type SearchResult struct {
	Title   string
	URL     string
	Content string
}

var numberedResultRE = regexp.MustCompile(`^(\d+)\.\s+(.*)$`)

// Searcher is the interface all web search backends must implement.
type Searcher interface {
	// Search executes a web search for the given query and returns matching results.
	Search(ctx context.Context, query string) ([]SearchResult, error)
}

// FormatResults converts search results into a plain-text block suitable for a tool response.
func FormatResults(results []SearchResult) string {
	if len(results) == 0 {
		return "No results found."
	}

	var sb strings.Builder
	for i, r := range results {
		fmt.Fprintf(&sb, "%d. %s\n   URL: %s\n   %s\n\n", i+1, r.Title, r.URL, r.Content)
	}

	return strings.TrimSpace(sb.String())
}

// ParseFormattedResults decodes the stable plain-text tool format back into results.
func ParseFormattedResults(raw string) []SearchResult {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "No results found." {
		return nil
	}

	blocks := strings.Split(raw, "\n\n")
	results := make([]SearchResult, 0, len(blocks))
	for _, block := range blocks {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if len(lines) < 2 {
			continue
		}

		titleLine := strings.TrimSpace(lines[0])
		match := numberedResultRE.FindStringSubmatch(titleLine)
		if len(match) != 3 {
			continue
		}

		urlLine := strings.TrimSpace(lines[1])
		if !strings.HasPrefix(urlLine, "URL: ") {
			continue
		}

		result := SearchResult{
			Title: strings.TrimSpace(match[2]),
			URL:   strings.TrimSpace(strings.TrimPrefix(urlLine, "URL: ")),
		}
		if len(lines) > 2 {
			result.Content = strings.TrimSpace(strings.Join(lines[2:], "\n"))
		}
		results = append(results, result)
	}

	return results
}

// NewHandler returns a handler that executes web searches via the provided searcher.
func NewHandler(searcher Searcher, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		query := ""
		if q, ok := args["query"]; ok {
			query, _ = q.(string)
		}

		if query == "" {
			return "", fmt.Errorf("query parameter was empty")
		}

		meta := toolctx.MetadataFromContext(ctx)
		log.Agent("agent.tool.web.search", meta.RequestID, meta.SessionID, meta.SenderID, meta.Gateway, meta.Model).Debug(
			"agent.tool.web.search.start",
			"starting web search tool",
			config.F("tool_name", "web.search"),
			config.F("query_chars", len(query)),
		)

		results, err := searcher.Search(ctx, query)
		if err != nil {
			return "", fmt.Errorf("search failed: %w", err)
		}

		return FormatResults(results), nil
	}
}
