package web_search

import "github.com/jonahgcarpenter/oswald-ai/internal/llm"

// Name is the model-facing web search tool name.
const Name = "web_search"

// Definition returns the model-facing web search contract.
func Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        Name,
		Description: "Search the web for information. Returns up to 5 results by default with titles, URLs, and descriptions. The query is passed through to the configured backend, so operators such as site:domain, filetype:pdf, intitle:word, -term, and \"exact phrase\" may work when the backend supports them.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"query": {Type: "string", Description: "The search query to look up on the web. You may include backend-supported operators such as site:example.com, filetype:pdf, intitle:word, -term, or \"exact phrase\"."},
				"limit": {Type: "integer", Description: "Maximum number of results to return. Defaults to 5.", Default: 5, Minimum: floatPointer(1), Maximum: floatPointer(100)},
			},
			Required:             []string{"query"},
			AdditionalProperties: boolPointer(false),
		},
	}
}

func floatPointer(value float64) *float64 { return &value }
func boolPointer(value bool) *bool        { return &value }
