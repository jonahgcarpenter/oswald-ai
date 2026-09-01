package webfetch

// Response is the bounded model-facing result of one direct web fetch.
type Response struct {
	Notice      string `json:"notice"`
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	ContentType string `json:"content_type"`
	Source      string `json:"source"`
	Content     string `json:"content"`
	IsTruncated bool   `json:"is_truncated"`
	IsDegraded  bool   `json:"is_degraded"`
}

type oEmbedResponse struct {
	AuthorName string `json:"author_name"`
	HTML       string `json:"html"`
}
