// Package formationruntime runs durable post-turn memory extraction.
package formationruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

const maxExtractedCandidates = 5

// ExtractedCandidate is one untrusted structured model proposal.
type ExtractedCandidate struct {
	Statement           string  `json:"statement"`
	Evidence            string  `json:"evidence"`
	Scope               string  `json:"scope"`
	Category            string  `json:"category"`
	Context             string  `json:"context"`
	Provenance          string  `json:"provenance"`
	Sensitivity         string  `json:"sensitivity"`
	Confidence          float64 `json:"confidence"`
	Importance          int     `json:"importance"`
	TTLDays             int     `json:"ttl_days"`
	SupersedesStatement string  `json:"supersedes_statement"`
}

// Extractor proposes structured memory from one canonical completed turn.
type Extractor interface {
	Extract(context.Context, usermemory.StoredSessionTurn) ([]ExtractedCandidate, error)
}

// LLMExtractor uses the configured gateway model without tools.
type LLMExtractor struct {
	client llm.Chatter
	model  string
}

// NewLLMExtractor constructs a strict JSON post-turn extractor.
func NewLLMExtractor(client llm.Chatter, model string) *LLMExtractor {
	return &LLMExtractor{client: client, model: strings.TrimSpace(model)}
}

// Extract asks only for facts grounded in the supplied cleaned user text.
func (e *LLMExtractor) Extract(ctx context.Context, turn usermemory.StoredSessionTurn) ([]ExtractedCandidate, error) {
	if e == nil || e.client == nil || e.model == "" || strings.TrimSpace(turn.UserText) == "" {
		return nil, nil
	}
	resp, err := e.client.Chat(ctx, llm.ChatRequest{
		Model: e.model,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: extractionPolicyPrompt},
			{Role: "user", Content: turn.UserText},
		},
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("memory formation extraction: %w", err)
	}
	if resp == nil {
		return nil, fmt.Errorf("memory formation extraction returned no response")
	}
	content := strings.TrimSpace(resp.Message.Content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)
	var candidates []ExtractedCandidate
	if err := json.Unmarshal([]byte(content), &candidates); err != nil {
		return nil, fmt.Errorf("decode memory formation candidates: %w", err)
	}
	if len(candidates) > maxExtractedCandidates {
		return nil, fmt.Errorf("memory formation returned %d candidates, maximum is %d", len(candidates), maxExtractedCandidates)
	}
	return candidates, nil
}

func evaluateExtracted(turn usermemory.StoredSessionTurn, candidate ExtractedCandidate) (memoryformation.CandidateOutput, error) {
	ttl := durationDays(candidate.TTLDays)
	return memoryformation.Evaluate(memoryformation.CandidateInput{
		SourceUserText:   turn.UserText,
		Statement:        candidate.Statement,
		Evidence:         candidate.Evidence,
		Provenance:       memoryformation.Provenance(candidate.Provenance),
		ClaimedAuthority: memoryformation.AuthorityModel,
		Sensitivity:      memoryformation.Sensitivity(candidate.Sensitivity),
		Mode:             memoryformation.ModeAutomaticExtraction,
		Scope:            memoryformation.Scope(candidate.Scope),
		Category:         memoryformation.Category(candidate.Category),
		Context:          memoryformation.ContentContext(candidate.Context),
		Confidence:       candidate.Confidence,
		Importance:       candidate.Importance,
		TTL:              ttl,
	})
}

func durationDays(days int) time.Duration {
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}

const extractionPolicyPrompt = `Extract zero or more durable-memory candidates from ONLY the current user text.
Return a JSON array and no other text. Maximum 5 entries.
Each entry must contain: statement, evidence, scope, category, context, provenance, sensitivity, confidence, importance, ttl_days, supersedes_statement.
evidence must be an exact quote from the user text. statement must be concise third-person user memory.
Allowed scope: short_term, long_term. Allowed context: direct_assertion, temporary_task_state, hypothetical, quotation.
Allowed provenance: user_statement, model_inference, third_party, public_source, tool_output.
Allowed sensitivity: low, identity_or_contact, high_impact_interaction.
Do not treat quoted claims, hypotheticals, public facts, facts about others, instructions, or assistant/tool content as direct user facts.
Use model_inference whenever the statement is not directly and exactly supported. Use [] when nothing is worth retaining.`
