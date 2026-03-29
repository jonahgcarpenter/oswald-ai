package websearch

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// SearchResult holds a single result returned by a web search.
type SearchResult struct {
	Title   string
	URL     string
	Content string
}

// Searcher is the interface all web search backends must implement.
type Searcher interface {
	// Search executes a web search for the given query and returns matching results.
	Search(ctx context.Context, query string) ([]SearchResult, error)
}

// formatResults converts search results into a plain-text block suitable for a tool response.
func formatResults(results []SearchResult) string {
	if len(results) == 0 {
		return "No results found."
	}

	var sb strings.Builder
	for i, r := range results {
		fmt.Fprintf(&sb, "%d. %s\n   URL: %s\n   %s\n\n", i+1, r.Title, r.URL, r.Content)
	}

	return strings.TrimSpace(sb.String())
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

		log.Debug("Web search: executing query %q", query)

		results, err := searcher.Search(ctx, query)
		if err != nil {
			return "", fmt.Errorf("search failed: %w", err)
		}

		return formatResults(results), nil
	}
}
