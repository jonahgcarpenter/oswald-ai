package searxng

// searxngResponse is the top-level JSON response from the SearXNG /search endpoint.
type searxngResponse struct {
	Results []searxngResult `json:"results"`
}

// searxngResult represents a single search result entry in the SearXNG JSON response.
type searxngResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"` // snippet or description
}
