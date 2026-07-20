// Package formationruntime runs durable post-turn memory extraction.
package formationruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

const maxExtractedCandidates = 5

var errPermanentExtraction = errors.New("permanent memory formation extraction failure")

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
	ClaimSlot           string  `json:"claim_slot"`
	ClaimValue          string  `json:"claim_value"`
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
		Model:  e.model,
		Format: "json",
		Messages: []llm.ChatMessage{
			{Role: "system", Content: extractionPolicyPrompt},
			{Role: "user", Content: turn.UserText},
		},
	}, nil)
	if err != nil {
		var httpErr *llm.ChatHTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode >= http.StatusBadRequest && httpErr.StatusCode < http.StatusInternalServerError && httpErr.StatusCode != http.StatusRequestTimeout && httpErr.StatusCode != http.StatusTooEarly && httpErr.StatusCode != http.StatusTooManyRequests {
			return nil, errors.Join(errPermanentExtraction, fmt.Errorf("memory formation extraction: %w", err))
		}
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
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var artifact struct {
		Candidates []json.RawMessage `json:"candidates"`
	}
	if err := decoder.Decode(&artifact); err != nil {
		return nil, errors.Join(errPermanentExtraction, fmt.Errorf("decode memory formation artifact: %w", err))
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.Join(errPermanentExtraction, fmt.Errorf("decode memory formation artifact: trailing JSON"))
	}
	if len(artifact.Candidates) > maxExtractedCandidates {
		return nil, errors.Join(errPermanentExtraction, fmt.Errorf("memory formation returned %d candidates, maximum is %d", len(artifact.Candidates), maxExtractedCandidates))
	}
	candidates := make([]ExtractedCandidate, 0, len(artifact.Candidates))
	for _, raw := range artifact.Candidates {
		var candidate ExtractedCandidate
		if err := json.Unmarshal(raw, &candidate); err != nil {
			continue
		}
		candidates = append(candidates, candidate)
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
		ClaimSlot:        candidate.ClaimSlot,
		ClaimValue:       candidate.ClaimValue,
	})
}

func durationDays(days int) time.Duration {
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}

const extractionPolicyPrompt = `Extract zero or more durable-memory candidates from ONLY the current user text.
Return exactly one JSON object shaped as {"candidates":[]} and no other text. Maximum 5 candidates.
Each entry must contain: statement, evidence, scope, category, context, provenance, sensitivity, confidence, importance, ttl_days, supersedes_statement, claim_slot, claim_value.
evidence must be an exact quote from the user text. For a direct first-person fact, quote the smallest complete span that states the fact and use provenance user_statement; the evidence may be part of a longer user prompt. statement must be concise third-person user memory.
Allowed scope: short_term, long_term. Allowed context: direct_assertion, temporary_task_state, hypothetical, quotation.
Allowed category: identity, communication_preferences, durable_preferences, projects, relationships, environment, notes.
Allowed provenance: user_statement, model_inference, third_party, public_source, tool_output.
Allowed sensitivity: low, identity_or_contact, high_impact_interaction.
confidence must be a number from 0 to 1. Use 0.95-1.0 only for explicit unqualified first-person facts, 0.70-0.89 for strong operational evidence, 0.45-0.69 for plausible indirect signals, and omit candidates below 0.35. Sensitivity does not lower confidence. importance must be an integer from 1 to 5. Direct identity facts, including names, must have importance at least 3.
Use short_term only with temporary_task_state and ttl_days from 1 to 30. Otherwise use long_term and ttl_days 0.
supersedes_statement must be a string; use an empty string when there is no replacement.
claim_slot is a stable dotted semantic property such as environment.linux_distribution. claim_value is a concise normalized value such as arch_family. Semantically equivalent evidence must reuse the same slot and value.
Do not retain standalone facts about the assistant, Oswald, system versions, quoted claims, hypotheticals, public facts, facts about others, instructions, or assistant/tool content. A direct first-person identity or relationship claim such as "I am your creator" is about the user and may use provenance user_statement.
Use model_inference whenever the statement is not directly and exactly supported. Inference evidence must be the complete user turn, never a partial span, and the statement must use cautious language such as may or possibly.
Example input: "what is the best pacman package for file management?". A valid candidate is "The user may use or be evaluating a pacman-based Linux environment." with provenance model_inference, confidence about 0.55, claim_slot environment.linux_distribution, and claim_value arch_family. Do not claim that the user definitely uses Arch.
Use {"candidates":[]} when nothing is worth retaining.`
