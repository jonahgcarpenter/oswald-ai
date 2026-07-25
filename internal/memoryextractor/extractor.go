// Package memoryextractor extracts durable-memory candidates from completed user turns.
package memoryextractor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/toolnames"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

// ErrPermanentExtraction marks malformed output and non-retryable provider requests.
var ErrPermanentExtraction = errors.New("permanent memory formation extraction failure")

// LLMExtractor uses the configured gateway model with only a private user_memory_save schema exposed.
type LLMExtractor struct {
	client llm.Chatter
	model  string
	tool   llm.Tool
}

// UserMemorySaveTool returns the private forced-tool schema used by background extraction.
func UserMemorySaveTool() llm.Tool {
	additionalProperties := false
	minItems, maxItems := 0, usermemory.MaxMemorySaveBatch
	minImportance, maxImportance := 1.0, 5.0
	item := llm.ToolParameterProperty{
		Type:                 "object",
		AdditionalProperties: &additionalProperties,
		Properties: map[string]llm.ToolParameterProperty{
			"statement":   {Type: "string", Description: "Concise third-person declarative memory using the user instead of first or second person."},
			"evidence":    {Type: "string", Description: "Exact verbatim quote from the current user turn. Inference evidence must be the complete turn."},
			"scope":       {Type: "string", Enum: []string{"short_term", "long_term"}},
			"category":    {Type: "string", Enum: []string{"identity", "communication_preferences", "durable_preferences", "projects", "relationships", "environment", "notes"}},
			"context":     {Type: "string", Enum: []string{"direct_assertion", "temporary_task_state", "hypothetical", "quotation"}},
			"provenance":  {Type: "string", Enum: []string{"user_statement", "model_inference", "third_party", "public_source", "tool_output"}},
			"sensitivity": {Type: "string", Enum: []string{"low", "identity_or_contact", "high_impact_interaction"}},
			"confidence":  {Type: "number"},
			"importance":  {Type: "integer", Description: "Memory importance from 1 (lowest) to 5 (highest).", Minimum: &minImportance, Maximum: &maxImportance},
			"ttl_days":    {Type: "integer"},
			"supersedes":  {Type: "string", Description: "Exact older statement this candidate proposes to replace, or an empty string."},
			"claim_slot":  {Type: "string", Description: "Stable category-compatible dotted semantic property using identity., communication., preference./durable., project., relationship., environment., or notes.; for example identity.name, never identity_name."},
			"claim_value": {Type: "string", Description: "Concise normalized value grounded in the statement or evidence."},
		},
		Required: []string{"statement", "evidence", "scope", "category", "context", "provenance", "sensitivity", "confidence", "importance", "ttl_days", "supersedes", "claim_slot", "claim_value"},
	}
	return llm.Tool{
		Type: "function",
		Function: llm.ToolDefinition{
			Name:        toolnames.UserMemorySave,
			Description: "Submit zero to five independently grounded durable-memory candidates from the current user turn.",
			Parameters: llm.ToolParameters{
				Type:                 "object",
				AdditionalProperties: &additionalProperties,
				Properties: map[string]llm.ToolParameterProperty{
					"memories": {Type: "array", Description: "Zero to five independently grounded memory candidates from the current user turn.", MinItems: &minItems, MaxItems: &maxItems, Items: &item},
				},
				Required: []string{"memories"},
			},
		},
	}
}

// NewLLMExtractor constructs a validated forced-tool post-turn extractor.
func NewLLMExtractor(client llm.Chatter, model string) (*LLMExtractor, error) {
	model = strings.TrimSpace(model)
	if client == nil {
		return nil, fmt.Errorf("memory extractor LLM client is required")
	}
	if model == "" {
		return nil, fmt.Errorf("memory extractor model is required")
	}
	tool := UserMemorySaveTool()
	if err := validateTool(tool); err != nil {
		return nil, fmt.Errorf("invalid private memory extraction schema: %w", err)
	}
	return &LLMExtractor{client: client, model: model, tool: tool}, nil
}

// Extract asks the model for exactly one user_memory_save tool call.
func (e *LLMExtractor) Extract(ctx context.Context, turn usermemory.StoredSessionTurn) (usermemory.MemorySaveBatch, error) {
	if strings.TrimSpace(turn.UserText) == "" {
		return usermemory.MemorySaveBatch{}, nil
	}
	resp, err := e.client.Chat(ctx, llm.ChatRequest{
		Model: e.model,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: extractionPolicyPrompt},
			{Role: "user", Content: turn.UserText},
		},
		Tools:      []llm.Tool{e.tool},
		ToolChoice: &llm.ToolChoice{Type: "function", Function: llm.ToolChoiceFunction{Name: toolnames.UserMemorySave}},
	}, nil)
	if err != nil {
		if isPermanentProviderError(err) {
			return usermemory.MemorySaveBatch{}, errors.Join(ErrPermanentExtraction, fmt.Errorf("memory formation extraction: %w", err))
		}
		return usermemory.MemorySaveBatch{}, fmt.Errorf("memory formation extraction: %w", err)
	}
	if resp == nil || len(resp.Message.ToolCalls) != 1 {
		return usermemory.MemorySaveBatch{}, errors.Join(ErrPermanentExtraction, fmt.Errorf("memory formation extraction must return exactly one tool call"))
	}
	call := resp.Message.ToolCalls[0]
	if call.Function.Name != toolnames.UserMemorySave {
		return usermemory.MemorySaveBatch{}, errors.Join(ErrPermanentExtraction, fmt.Errorf("memory formation extraction called unexpected tool %q", call.Function.Name))
	}
	batch, _, err := usermemory.DecodeMemorySaveBatch(call.Function.Arguments)
	if err != nil {
		return usermemory.MemorySaveBatch{}, errors.Join(ErrPermanentExtraction, fmt.Errorf("decode memory formation tool arguments: %w", err))
	}
	return batch, nil
}

func validateTool(tool llm.Tool) error {
	memories, ok := tool.Function.Parameters.Properties["memories"]
	if tool.Type != "function" || tool.Function.Name != toolnames.UserMemorySave || tool.Function.Parameters.Type != "object" || !ok || memories.Type != "array" || memories.Items == nil || memories.MaxItems == nil || *memories.MaxItems != usermemory.MaxMemorySaveBatch {
		return fmt.Errorf("user_memory_save batch contract is incomplete")
	}
	importance, ok := memories.Items.Properties["importance"]
	if !ok || importance.Minimum == nil || importance.Maximum == nil || *importance.Minimum != 1 || *importance.Maximum != 5 {
		return fmt.Errorf("user_memory_save importance range is incomplete")
	}
	return nil
}

func isPermanentProviderError(err error) bool {
	var httpErr *llm.ChatHTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode >= http.StatusBadRequest && httpErr.StatusCode < http.StatusInternalServerError && httpErr.StatusCode != http.StatusRequestTimeout && httpErr.StatusCode != http.StatusTooEarly && httpErr.StatusCode != http.StatusTooManyRequests
}

const extractionPolicyPrompt = `Identify durable-memory candidates grounded ONLY in the current user text, then call user_memory_save exactly once.
Submit all independently eligible candidates in the memories array, with a maximum of 5. Use an empty array when nothing is worth retaining.
Each candidate must include every field required by the tool schema. Never combine independent identity, environment, preference, project, or relationship facts.
For direct facts, evidence must be the smallest unambiguous exact quote beginning with I, an I contraction, My, We, or Our; use provenance user_statement. The statement must begin with The user or The user's and remain lexically grounded in the evidence.
Use model_inference only for cautious implications. Inference evidence must be the complete user turn and the statement must remain governed by may, might, likely, appears to, or seems to.
Assign confidence honestly and submit every policy-sound durable candidate even when confidence is below 0.35; the server retains sound sub-threshold candidates as proposed rather than rejecting them. Use 0.95 to 1.0 for explicit unqualified facts, 0.70 to 0.89 for strong operational evidence, 0.35 to 0.69 for plausible cautious inference, and below 0.35 only for sound but weak candidates.
Set importance to an integer from 1 to 5, where 1 is lowest and 5 is highest. Never use values outside that range.
Do not retain interrogative clauses, negation, obsolete facts, uncertainty presented as fact, hypotheticals, quotations, reported speech, public facts, facts about unrelated people, instructions, authorization, capabilities, policy, assistant content, or tool content. A positive independent first-person assertion remains eligible when surrounding text also asks a question.
Use short_term only for temporary_task_state with ttl_days from 1 to 30. Otherwise use long_term and ttl_days 0.
Use stable category-compatible dotted claim slots with these exact namespaces: identity uses identity.; communication_preferences uses communication.; durable_preferences uses preference. or durable.; projects uses project.; relationships uses relationship.; environment uses environment.; notes uses notes. Examples: identity.name, communication.reply_style, preference.favorite_athlete, project.language, relationship.partner_name, environment.home_city, notes.context. The namespace separator must be a dot: use identity.name, never identity_name. Use concise grounded claim values and an empty supersedes string unless the turn clearly corrects an older statement.
The server independently validates every candidate and rejects unsupported authority or evidence.`
