package usermemory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
)

const MaxMemorySaveBatch = 5

// MemorySaveItem is the shared untrusted candidate shape used by foreground
// tool calls and background extraction.
type MemorySaveItem struct {
	InputIndex  int     `json:"-"`
	Statement   string  `json:"statement"`
	Evidence    string  `json:"evidence"`
	Scope       string  `json:"scope"`
	Category    string  `json:"category"`
	Context     string  `json:"context"`
	Provenance  string  `json:"provenance"`
	Sensitivity string  `json:"sensitivity"`
	Confidence  float64 `json:"confidence"`
	Importance  int     `json:"importance"`
	TTLDays     int     `json:"ttl_days"`
	Supersedes  string  `json:"supersedes"`
	ClaimSlot   string  `json:"claim_slot"`
	ClaimValue  string  `json:"claim_value"`
}

// MemorySaveBatch is the single shared input contract for memory formation.
type MemorySaveBatch struct {
	Memories []MemorySaveItem `json:"memories"`
}

// MemorySaveOutcome reports one independently evaluated batch item.
type MemorySaveOutcome struct {
	InputIndex  int
	CandidateID int64
	State       string
	Reason      string
	Err         error
	Operational bool
}

// MemorySaveItemError identifies one malformed item without rejecting valid siblings.
type MemorySaveItemError struct {
	InputIndex int
	Err        error
}

func (e MemorySaveItemError) Error() string {
	return e.Err.Error()
}

// DecodeMemorySaveBatch strictly decodes the outer object while dropping only
// malformed individual items so valid siblings can still be evaluated.
func DecodeMemorySaveBatch(arguments map[string]interface{}) (MemorySaveBatch, []MemorySaveItemError, error) {
	if arguments == nil {
		return MemorySaveBatch{}, nil, fmt.Errorf("memories is required")
	}
	for key := range arguments {
		if key != "memories" {
			return MemorySaveBatch{}, nil, fmt.Errorf("unknown batch field %q", key)
		}
	}
	rawItems, ok := arguments["memories"].([]interface{})
	if !ok {
		return MemorySaveBatch{}, nil, fmt.Errorf("memories must be an array")
	}
	if len(rawItems) > MaxMemorySaveBatch {
		return MemorySaveBatch{}, nil, fmt.Errorf("memories contains %d items; maximum is %d", len(rawItems), MaxMemorySaveBatch)
	}
	batch := MemorySaveBatch{Memories: make([]MemorySaveItem, 0, len(rawItems))}
	itemErrors := make([]MemorySaveItemError, 0)
	for index, raw := range rawItems {
		encoded, err := json.Marshal(raw)
		if err != nil {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: fmt.Errorf("encode item: %w", err)})
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.DisallowUnknownFields()
		var item MemorySaveItem
		if err := decoder.Decode(&item); err != nil {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: err})
			continue
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: fmt.Errorf("trailing JSON")})
			continue
		}
		if strings.TrimSpace(item.Statement) == "" || strings.TrimSpace(item.Evidence) == "" || strings.TrimSpace(item.ClaimSlot) == "" || strings.TrimSpace(item.ClaimValue) == "" {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: fmt.Errorf("statement, evidence, claim_slot, and claim_value are required")})
			continue
		}
		item.InputIndex = index
		batch.Memories = append(batch.Memories, item)
	}
	return batch, itemErrors, nil
}

// SubmitMemorySaveBatch evaluates and stages each item independently through
// the canonical candidate lifecycle.
func (s *Store) SubmitMemorySaveBatch(ctx context.Context, userID, sourceText string, source FormationSource, batch MemorySaveBatch, formationJob *FormationJob) []MemorySaveOutcome {
	outcomes := make([]MemorySaveOutcome, 0, len(batch.Memories))
	remembered, hasExplicitIntent := memoryformation.ParseExplicitRemember(sourceText)
	for _, item := range batch.Memories {
		mode := memoryformation.ModeAutomaticExtraction
		if hasExplicitIntent && strings.Contains(normalizedMemoryText(remembered), normalizedMemoryText(item.Evidence)) {
			mode = memoryformation.ModeExplicitRemember
		}
		output, err := memoryformation.Evaluate(memoryformation.CandidateInput{
			SourceUserText:   sourceText,
			Statement:        item.Statement,
			Evidence:         item.Evidence,
			Provenance:       memoryformation.Provenance(item.Provenance),
			ClaimedAuthority: claimedAuthority(item.Provenance),
			Sensitivity:      memoryformation.Sensitivity(item.Sensitivity),
			Mode:             mode,
			Scope:            memoryformation.Scope(item.Scope),
			Category:         memoryformation.Category(item.Category),
			Context:          memoryformation.ContentContext(item.Context),
			Confidence:       item.Confidence,
			Importance:       item.Importance,
			TTL:              durationFromDays(item.TTLDays),
			ClaimSlot:        item.ClaimSlot,
			ClaimValue:       item.ClaimValue,
		})
		if err != nil {
			outcomes = append(outcomes, MemorySaveOutcome{InputIndex: item.InputIndex, Err: err})
			continue
		}
		candidate, _, err := s.ProposeCandidate(ctx, userID, CandidateProposal{
			Output: output, Source: source, SupersedesStatement: item.Supersedes, FormationJob: formationJob,
		})
		if err != nil {
			outcomes = append(outcomes, MemorySaveOutcome{InputIndex: item.InputIndex, Err: err, Operational: true})
			continue
		}
		outcomes = append(outcomes, MemorySaveOutcome{InputIndex: item.InputIndex, CandidateID: candidate.ID, State: candidate.State, Reason: candidate.DecisionReason})
	}
	return outcomes
}

func claimedAuthority(provenance string) memoryformation.SourceAuthority {
	switch memoryformation.Provenance(provenance) {
	case memoryformation.ProvenanceUserStatement:
		return memoryformation.AuthorityUserDirect
	case memoryformation.ProvenanceThirdParty:
		return memoryformation.AuthorityThirdParty
	case memoryformation.ProvenancePublicSource:
		return memoryformation.AuthorityPublic
	case memoryformation.ProvenanceToolOutput:
		return memoryformation.AuthorityTool
	default:
		return memoryformation.AuthorityModel
	}
}

func durationFromDays(days int) time.Duration {
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}

func normalizedMemoryText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}
