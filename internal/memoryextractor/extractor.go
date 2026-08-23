// Package memoryextractor extracts durable-memory candidates from completed user turns.
package memoryextractor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

const (
	userMemorySaveToolName  = "user_memory_save"
	maxExtractedMemoryBatch = 5
	extractionMaxTokens     = 2048

	invalidOutputMissingToolCall    = "missing_tool_call"
	invalidOutputMultipleToolCalls  = "multiple_tool_calls"
	invalidOutputUnexpectedToolCall = "unexpected_tool_call"
	invalidOutputMalformedArguments = "malformed_tool_arguments"
	invalidOutputBatchShape         = "invalid_batch_shape"
	invalidOutputAllItemsMalformed  = "all_items_malformed"
)

// ErrPermanentExtraction marks malformed output and non-retryable provider requests.
var ErrPermanentExtraction = errors.New("permanent memory formation extraction failure")

// ErrInvalidOutput marks model output that may succeed on one bounded retry.
var ErrInvalidOutput = errors.New("invalid memory formation model output")

// InvalidOutputError identifies one stable model-output failure category.
type InvalidOutputError struct {
	Code string
}

func (e *InvalidOutputError) Error() string {
	return "invalid memory formation model output: " + e.Code
}

func (e *InvalidOutputError) Unwrap() error {
	return ErrInvalidOutput
}

// InvalidOutputCode returns the stable reason carried by an invalid-output error.
func InvalidOutputCode(err error) (string, bool) {
	var outputErr *InvalidOutputError
	if !errors.As(err, &outputErr) {
		return "", false
	}
	return outputErr.Code, true
}

func invalidOutput(code string) error {
	return &InvalidOutputError{Code: code}
}

// LLMExtractor uses the configured gateway model with only a private user_memory_save schema exposed.
type LLMExtractor struct {
	client llm.Chatter
	model  string
	tool   llm.Tool
}

func userMemorySaveTool() llm.Tool {
	additionalProperties := false
	minItems, maxItems := 0, maxExtractedMemoryBatch
	minConfidence, maxConfidence := 0.0, 1.0
	minImportance, maxImportance := 1.0, 5.0
	minTTLDays, maxTTLDays := 0.0, 30.0
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
			"confidence":  {Type: "number", Minimum: &minConfidence, Maximum: &maxConfidence},
			"importance":  {Type: "integer", Description: "Memory importance from 1 (lowest) to 5 (highest).", Minimum: &minImportance, Maximum: &maxImportance},
			"ttl_days":    {Type: "integer", Minimum: &minTTLDays, Maximum: &maxTTLDays},
			"supersedes":  {Type: "string", Description: "Exact older statement this candidate proposes to replace, or an empty string."},
			"claim_slot":  {Type: "string", Description: "Stable category-compatible dotted semantic property using identity., communication., preference./durable., project., relationship., environment., or notes.; for example identity.name, never identity_name."},
			"claim_value": {Type: "string", Description: "Concise normalized value grounded in the statement or evidence."},
		},
		Required: []string{"statement", "evidence", "scope", "category", "context", "provenance", "sensitivity", "confidence", "importance", "ttl_days", "supersedes", "claim_slot", "claim_value"},
	}
	return llm.Tool{
		Type: "function",
		Function: llm.ToolDefinition{
			Name:        userMemorySaveToolName,
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
	tool := userMemorySaveTool()
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
	parallelToolCalls := false
	temperature := 0.0
	resp, err := e.client.Chat(ctx, llm.ChatRequest{
		Model: e.model,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: extractionPolicyPrompt},
			{Role: "user", Content: turn.UserText},
		},
		Tools:             []llm.Tool{e.tool},
		ToolChoice:        llm.ToolChoiceRequired,
		ParallelToolCalls: &parallelToolCalls,
		Temperature:       &temperature,
		MaxTokens:         extractionMaxTokens,
	}, nil)
	if err != nil {
		if isPermanentProviderError(err) {
			return usermemory.MemorySaveBatch{}, errors.Join(ErrPermanentExtraction, fmt.Errorf("memory formation extraction: %w", err))
		}
		return usermemory.MemorySaveBatch{}, fmt.Errorf("memory formation extraction: %w", err)
	}
	if resp == nil || len(resp.Message.ToolCalls) == 0 {
		return usermemory.MemorySaveBatch{}, invalidOutput(invalidOutputMissingToolCall)
	}
	if len(resp.Message.ToolCalls) != 1 {
		return usermemory.MemorySaveBatch{}, invalidOutput(invalidOutputMultipleToolCalls)
	}
	call := resp.Message.ToolCalls[0]
	if call.Function.Name != userMemorySaveToolName {
		return usermemory.MemorySaveBatch{}, invalidOutput(invalidOutputUnexpectedToolCall)
	}
	if _, malformed := call.Function.Arguments["_raw"]; malformed {
		return usermemory.MemorySaveBatch{}, invalidOutput(invalidOutputMalformedArguments)
	}
	batch, itemErrors, err := usermemory.DecodeMemorySaveBatch(call.Function.Arguments)
	if err != nil {
		return usermemory.MemorySaveBatch{}, invalidOutput(invalidOutputBatchShape)
	}
	if len(itemErrors) > 0 && len(batch.Memories) == 0 {
		return usermemory.MemorySaveBatch{}, invalidOutput(invalidOutputAllItemsMalformed)
	}
	return batch, nil
}

func validateTool(tool llm.Tool) error {
	memories, ok := tool.Function.Parameters.Properties["memories"]
	if tool.Type != "function" || tool.Function.Name != userMemorySaveToolName || tool.Function.Parameters.Type != "object" || tool.Function.Parameters.AdditionalProperties == nil || *tool.Function.Parameters.AdditionalProperties || len(tool.Function.Parameters.Required) != 1 || tool.Function.Parameters.Required[0] != "memories" || !ok || memories.Type != "array" || memories.Items == nil || memories.MinItems == nil || *memories.MinItems != 0 || memories.MaxItems == nil || *memories.MaxItems != maxExtractedMemoryBatch {
		return fmt.Errorf("user_memory_save batch contract is incomplete")
	}
	item := memories.Items
	if item.Type != "object" || item.AdditionalProperties == nil || *item.AdditionalProperties || len(item.Required) != len(usermemory.MemorySaveRequiredFields()) {
		return fmt.Errorf("user_memory_save item contract is incomplete")
	}
	required := make(map[string]struct{}, len(item.Required))
	for _, field := range item.Required {
		required[field] = struct{}{}
	}
	for _, field := range usermemory.MemorySaveRequiredFields() {
		if _, ok := required[field]; !ok {
			return fmt.Errorf("user_memory_save required field %q is missing", field)
		}
		if _, ok := item.Properties[field]; !ok {
			return fmt.Errorf("user_memory_save property %q is missing", field)
		}
	}
	for field, bounds := range map[string][2]float64{"confidence": {0, 1}, "importance": {1, 5}, "ttl_days": {0, 30}} {
		property := item.Properties[field]
		if property.Minimum == nil || property.Maximum == nil || *property.Minimum != bounds[0] || *property.Maximum != bounds[1] {
			return fmt.Errorf("user_memory_save %s range is incomplete", field)
		}
	}
	return nil
}

func isPermanentProviderError(err error) bool {
	var httpErr *llm.ChatHTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode >= http.StatusBadRequest && httpErr.StatusCode < http.StatusInternalServerError && httpErr.StatusCode != http.StatusRequestTimeout && httpErr.StatusCode != http.StatusTooEarly && httpErr.StatusCode != http.StatusTooManyRequests
}

const extractionPolicyPrompt = `Extract retained user-memory candidates from ONLY the current user text, then call user_memory_save exactly once.

OUTPUT CONTRACT
Return one arguments object with a memories array containing 0 to 5 independently eligible candidates. Use {"memories":[]} when nothing qualifies. Never invent a candidate to avoid an empty array. Keep independent identity, communication preference, durable preference, project, relationship, environment, note, or temporary-task facts in separate candidates.

Every candidate must contain all 13 fields: statement, evidence, scope, category, context, provenance, sensitivity, confidence, importance, ttl_days, supersedes, claim_slot, claim_value.

ELIGIBILITY
Do not retain interrogative claims, negation, obsolete facts, unsupported certainty, hypotheticals, quotations, reported speech, public facts, facts centered on unrelated people, credentials, instructions, authorization, capabilities, policy, assistant content, or tool content. A question, request, or topic mention does not establish that the user uses, prefers, owns, or is working on the topic. A positive independent first-person assertion may qualify when surrounding text also asks a question.

DIRECT FACTS
Use provenance "user_statement" and context "direct_assertion". Evidence must be the smallest unambiguous exact quote from the current user text beginning with I, an I contraction, My, We, a We contraction, or Our. Statement must be concise, begin with "The user" or "The user's", and remain lexically grounded in the evidence.

INFERENCE
Use provenance "model_inference" and context "direct_assertion" only for a cautious user-centered implication. Evidence must be the complete user turn. Statement must remain governed by "may", "might", "likely", "appears to", or "seems to". Do not infer a preference, project, identity, or environment from a question or topic mention.

LIFETIME
Use scope "short_term", context "temporary_task_state", and ttl_days 1 through 30 only for bounded temporary task state. Otherwise use scope "long_term" and ttl_days 0.

CATEGORY AND CLAIM SLOT
Category and claim_slot must match: identity -> identity.*; communication_preferences -> communication.*; durable_preferences -> preference.* or durable.*; projects -> project.*; relationships -> relationship.*; environment -> environment.*; notes -> notes.*. Use a stable dotted slot such as identity.name, never identity_name. claim_value must be concise and grounded in the evidence. Use supersedes "" unless the current text itself supplies an unambiguous correction and the exact older statement.

SENSITIVITY
Use "identity_or_contact" for identity or contact facts, "high_impact_interaction" for high-impact interaction facts, and "low" otherwise.

SCORING
Assign confidence only after eligibility: 0.95-1.0 for an explicit unqualified fact; 0.70-0.89 for strong indirect operational evidence; 0.35-0.69 for plausible cautious inference; below 0.35 only for a sound but weak candidate. Submit eligible sub-threshold candidates; the server records them as proposed. Never lower confidence to make ineligible content eligible. importance must be an integer from 1 to 5.

Complete shape example for current text "I prefer concise replies.":
{"memories":[{"statement":"The user prefers concise replies.","evidence":"I prefer concise replies.","scope":"long_term","category":"communication_preferences","context":"direct_assertion","provenance":"user_statement","sensitivity":"low","confidence":0.98,"importance":4,"ttl_days":0,"supersedes":"","claim_slot":"communication.reply_style","claim_value":"concise"}]}

Empty example for current text "What is a closure in Go?":
{"memories":[]}

Examples demonstrate output shape only. Extract only facts independently supported by the actual current user text. The server independently validates every candidate and rejects unsupported evidence, authority, classification, or claim identity.`
