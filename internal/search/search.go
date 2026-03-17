package search

import "context"

// SearchResult holds a single result returned by a web search.
type SearchResult struct {
	Title   string
	URL     string
	Content string // snippet or description from the search engine
}

// Searcher is the interface all search backends must implement.
// Implementations handle query encoding, HTTP communication, and result parsing.
type Searcher interface {
	// Search executes a web search for the given query and returns matching results.
	// Implementations must fail safely — on error, return empty results rather than
	// propagating failures that would break the query generator pipeline.
	Search(ctx context.Context, query string) ([]SearchResult, error)
}
