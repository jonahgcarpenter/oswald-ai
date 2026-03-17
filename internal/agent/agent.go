package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/provider"
	"github.com/jonahgcarpenter/oswald-ai/internal/search"
)

const (
	// webSearchToolName is the function name exposed to the query generator model.
	webSearchToolName = "web_search"
)

// ModelMetrics holds performance data from a single LLM call.
type ModelMetrics struct {
	Model              string  `json:"model"`
	TotalDuration      int64   `json:"total_duration_ms"`
	LoadDuration       int64   `json:"load_duration_ms"`
	PromptEvalDuration int64   `json:"prompt_eval_duration_ms"`
	EvalDuration       int64   `json:"eval_duration_ms"`
	TokensPerSecond    float64 `json:"tokens_per_second"`
}

// AgentResponse is the final payload returned to the gateway after processing.
type AgentResponse struct {
	Model         string        `json:"model"`
	Response      string        `json:"response,omitempty"`
	SearchSummary string        `json:"search_summary,omitempty"` // findings from the query generator; empty if no search was performed
	Error         string        `json:"error,omitempty"`
	Metrics       *ModelMetrics `json:"metrics,omitempty"`
}

// Agent handles all LLM orchestration: query generation with web search and final response.
type Agent struct {
	provider             provider.Provider
	searcher             search.Searcher
	queryModel           string
	querySystemPrompt    string
	responseModel        string
	responseSystemPrompt string
	maxIterations        int
	log                  *config.Logger
}

// NewAgent initializes the Agent with a provider, searcher, query worker, response worker,
// iteration cap, and logger. The query worker drives the agentic search loop; the response
// worker generates the final reply sent to the user.
func NewAgent(
	provider provider.Provider,
	searcher search.Searcher,
	queryWorker *WorkerAgent,
	responseWorker *WorkerAgent,
	maxIterations int,
	log *config.Logger,
) *Agent {
	return &Agent{
		provider:             provider,
		searcher:             searcher,
		queryModel:           queryWorker.ResolveModel(),
		querySystemPrompt:    queryWorker.SystemPrompt,
		responseModel:        responseWorker.ResolveModel(),
		responseSystemPrompt: responseWorker.SystemPrompt,
		maxIterations:        maxIterations,
		log:                  log,
	}
}

// truncate returns s shortened to at most max runes, appending "…" if cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// buildWebSearchTool returns the tool definition given to the query generator model.
func buildWebSearchTool() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolDefinition{
			Name:        webSearchToolName,
			Description: "Search the web for current or factual information. Use precise, targeted queries.",
			Parameters: provider.ToolParameters{
				Type: "object",
				Properties: map[string]provider.ToolParameterProperty{
					"query": {
						Type:        "string",
						Description: "The search query to execute",
					},
				},
				Required: []string{"query"},
			},
		},
	}
}

// formatSearchResults converts a slice of search results into a plain-text block
// suitable for injection as a tool response message.
func formatSearchResults(results []search.SearchResult) string {
	if len(results) == 0 {
		return "No results found."
	}
	var sb strings.Builder
	for i, r := range results {
		fmt.Fprintf(&sb, "%d. %s\n   URL: %s\n   %s\n\n", i+1, r.Title, r.URL, r.Content)
	}
	return strings.TrimSpace(sb.String())
}

// runQueryGenerator runs the agentic query generator loop. The small query model
// is given the web_search tool and the user's prompt. It may call the tool zero or
// more times (up to maxIterations) to gather search results, then produces a text
// summary of its findings. Returns an empty string if no search was needed.
//
// Search failures are handled gracefully: a "no results" tool response is injected
// so the model can decide how to proceed without crashing the pipeline.
func (a *Agent) runQueryGenerator(ctx context.Context, userPrompt string) string {
	messages := []provider.ChatMessage{
		{Role: "system", Content: a.querySystemPrompt},
		{Role: "user", Content: userPrompt},
	}

	req := provider.ChatRequest{
		Model:  a.queryModel,
		Stream: false,
		Tools:  []provider.Tool{buildWebSearchTool()},
	}

	for iteration := 1; iteration <= a.maxIterations; iteration++ {
		req.Messages = messages

		resp, err := a.provider.Chat(ctx, req, nil)
		if err != nil {
			// Hard provider error — log and abort the search loop entirely
			a.log.Warn("Query generator failed on iteration %d: %v", iteration, err)
			return ""
		}

		a.log.Debug("Query generator iteration %d/%d: tool_calls=%d content_len=%d",
			iteration, a.maxIterations, len(resp.Message.ToolCalls), len(resp.Message.Content))

		// No tool call means the model is done — either it has a summary or decided no search needed
		if len(resp.Message.ToolCalls) == 0 {
			summary := strings.TrimSpace(resp.Message.Content)
			if summary != "" {
				a.log.Info("Query generator: produced summary (%d chars)", len(summary))
			} else {
				a.log.Info("Query generator: no search needed")
			}
			return summary
		}

		// Process tool calls — append the assistant turn with the tool calls
		messages = append(messages, resp.Message)

		// Execute each tool call and append the results as tool response messages.
		// NOTE: Most small models only emit one tool call at a time, but we handle
		// multiple to be safe.
		for _, tc := range resp.Message.ToolCalls {
			if tc.Function.Name != webSearchToolName {
				// Unknown tool — inject an error response so the model can recover
				a.log.Warn("Query generator called unknown tool %q", tc.Function.Name)
				messages = append(messages, provider.ChatMessage{
					Role:     "tool",
					ToolName: tc.Function.Name,
					Content:  fmt.Sprintf("Error: unknown tool %q", tc.Function.Name),
				})
				continue
			}

			query := ""
			if q, ok := tc.Function.Arguments["query"]; ok {
				query, _ = q.(string)
			}

			if query == "" {
				a.log.Warn("Query generator called web_search with empty query")
				messages = append(messages, provider.ChatMessage{
					Role:     "tool",
					ToolName: webSearchToolName,
					Content:  "Error: query parameter was empty",
				})
				continue
			}

			a.log.Info("Query generator: searching for %q (iteration %d)", query, iteration)

			results, searchErr := a.searcher.Search(ctx, query)
			var toolContent string
			if searchErr != nil {
				// Fail safely: log the error, inject a "no results" response so the
				// model can decide whether to retry or summarize from what it knows
				a.log.Warn("SearXNG search failed for %q: %v", query, searchErr)
				toolContent = "Search failed: " + searchErr.Error() + ". No results available."
			} else {
				toolContent = formatSearchResults(results)
			}

			messages = append(messages, provider.ChatMessage{
				Role:     "tool",
				ToolName: webSearchToolName,
				Content:  toolContent,
			})
		}
	}

	// Max iterations reached — the model kept searching without producing a summary.
	// Return empty; the uncensored model will answer from its own knowledge.
	a.log.Warn("Query generator: max iterations (%d) reached without summary", a.maxIterations)
	return ""
}

// Process handles the end-to-end pipeline: query generation with optional web search,
// followed by the uncensored model generating the final response. Streams partial
// content via streamCallback if provided.
func (a *Agent) Process(userPrompt string, streamCallback func(chunk string)) (*AgentResponse, error) {
	a.log.Info("Processing request: %q", truncate(userPrompt, 100))

	// Run the query generator agentic loop (60s budget for search)
	ctxQuery, cancelQuery := context.WithTimeout(context.Background(), 60*time.Second)
	searchSummary := a.runQueryGenerator(ctxQuery, userPrompt)
	cancelQuery()

	// Build the prompt for the uncensored model.
	// If the query generator found relevant information, inject it as context above the
	// user's original prompt so the uncensored model can incorporate it naturally.
	responsePrompt := userPrompt
	if searchSummary != "" {
		responsePrompt = fmt.Sprintf("<search_context>\n%s\n</search_context>\n\n%s", searchSummary, userPrompt)
	}

	// Generate the final response from the uncensored model
	ctxGen, cancelGen := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelGen()

	isStreaming := streamCallback != nil

	responseMessages := []provider.ChatMessage{
		{Role: "system", Content: a.responseSystemPrompt},
		{Role: "user", Content: responsePrompt},
	}

	responseReq := provider.ChatRequest{
		Model:    a.responseModel,
		Messages: responseMessages,
		Stream:   isStreaming,
	}

	// Adapt gateway callback (func(string)) to Chat API callback (func(ChatMessage)).
	// Extract Content field only; thinking tokens are not streamed to gateways.
	// NOTE: Gateways expect plain text content only.
	var chatCallback func(provider.ChatMessage)
	if streamCallback != nil {
		chatCallback = func(chunk provider.ChatMessage) {
			if chunk.Content != "" {
				streamCallback(chunk.Content)
			}
		}
	}

	a.log.Debug("Response generation starting: model=%s search=%v", a.responseModel, searchSummary != "")

	expertResp, err := a.provider.Chat(ctxGen, responseReq, chatCallback)
	if err != nil {
		a.log.Error("Response model %s failed: %v", a.responseModel, err)
		return &AgentResponse{
			Model: a.responseModel,
			Error: fmt.Sprintf("Response model failed: %v", err),
		}, nil
	}

	a.log.Info("Response complete: model=%s", a.responseModel)

	return &AgentResponse{
		Model:         a.responseModel,
		Response:      expertResp.Message.Content,
		SearchSummary: searchSummary,
		Metrics:       mapMetrics(expertResp),
	}, nil
}

// mapMetrics converts a *provider.ChatResponse into a *ModelMetrics summary for reporting.
// Returns nil if the response is missing or has no evaluation duration (partial failure).
// Converts nanosecond timings to milliseconds and calculates tokens/second throughput.
func mapMetrics(resp *provider.ChatResponse) *ModelMetrics {
	if resp == nil || resp.EvalDuration <= 0 {
		return nil
	}
	tps := float64(resp.EvalCount) / (float64(resp.EvalDuration) / 1e9)
	return &ModelMetrics{
		Model:              resp.Model,
		TotalDuration:      resp.TotalDuration / 1e6,
		LoadDuration:       resp.LoadDuration / 1e6,
		PromptEvalDuration: resp.PromptEvalDuration / 1e6,
		EvalDuration:       resp.EvalDuration / 1e6,
		TokensPerSecond:    tps,
	}
}
