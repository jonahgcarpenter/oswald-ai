package llm

// ModelEntry is one model from a provider's models listing. Window is the
// reported context length; 0 when the provider omits it.
type ModelEntry struct {
	ID     string
	Window int
}
