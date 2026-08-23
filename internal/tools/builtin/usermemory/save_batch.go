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

const maxExtractedMemoryBatch = 5

var memorySaveRequiredFields = []string{
	"statement", "evidence", "scope", "category", "context", "provenance", "sensitivity",
	"confidence", "importance", "ttl_days", "supersedes", "claim_slot", "claim_value",
}

// MemorySaveRequiredFields returns the complete private extraction item contract.
func MemorySaveRequiredFields() []string {
	return append([]string(nil), memorySaveRequiredFields...)
}

// MemorySaveItem is the untrusted candidate shape used by background extraction.
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

// MemorySaveBatch is the input contract for background memory formation.
type MemorySaveBatch struct {
	Memories       []MemorySaveItem `json:"memories"`
	SubmittedCount int              `json:"-"`
	MalformedCount int              `json:"-"`
}

// MemorySaveOutcome reports one independently evaluated batch item.
type MemorySaveOutcome struct {
	InputIndex        int
	CandidateID       int64
	State             string
	PublishedMemoryID int64
	Reason            string
	Err               error
	Operational       bool
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
	if len(rawItems) > maxExtractedMemoryBatch {
		return MemorySaveBatch{}, nil, fmt.Errorf("memories contains %d items; maximum is %d", len(rawItems), maxExtractedMemoryBatch)
	}
	batch := MemorySaveBatch{Memories: make([]MemorySaveItem, 0, len(rawItems)), SubmittedCount: len(rawItems)}
	itemErrors := make([]MemorySaveItemError, 0)
	for index, raw := range rawItems {
		encoded, err := json.Marshal(raw)
		if err != nil {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: fmt.Errorf("encode item: %w", err)})
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil || fields == nil {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: fmt.Errorf("item must be an object")})
			continue
		}
		missing := ""
		for _, field := range memorySaveRequiredFields {
			value, ok := fields[field]
			if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				missing = field
				break
			}
		}
		if missing != "" {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: fmt.Errorf("%s is required", missing)})
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
		if strings.TrimSpace(item.Statement) == "" || strings.TrimSpace(item.Evidence) == "" || strings.TrimSpace(item.Scope) == "" || strings.TrimSpace(item.Category) == "" || strings.TrimSpace(item.Context) == "" || strings.TrimSpace(item.Provenance) == "" || strings.TrimSpace(item.Sensitivity) == "" || strings.TrimSpace(item.ClaimSlot) == "" || strings.TrimSpace(item.ClaimValue) == "" {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: fmt.Errorf("required string fields except supersedes must be non-empty")})
			continue
		}
		if item.Confidence < 0 || item.Confidence > 1 || item.Importance < 1 || item.Importance > 5 || item.TTLDays < 0 || item.TTLDays > 30 {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: fmt.Errorf("confidence, importance, or ttl_days is outside the schema range")})
			continue
		}
		if !validMemorySaveEnums(item) {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: fmt.Errorf("scope, category, context, provenance, or sensitivity is outside the schema enum")})
			continue
		}
		item.InputIndex = index
		batch.Memories = append(batch.Memories, item)
	}
	batch.MalformedCount = len(itemErrors)
	return batch, itemErrors, nil
}

func validMemorySaveEnums(item MemorySaveItem) bool {
	validScope := item.Scope == string(memoryformation.ScopeShortTerm) || item.Scope == string(memoryformation.ScopeLongTerm)
	validCategory := item.Category == string(memoryformation.CategoryIdentity) ||
		item.Category == string(memoryformation.CategoryCommunicationPreferences) ||
		item.Category == string(memoryformation.CategoryDurablePreferences) ||
		item.Category == string(memoryformation.CategoryProjects) ||
		item.Category == string(memoryformation.CategoryRelationships) ||
		item.Category == string(memoryformation.CategoryEnvironment) ||
		item.Category == string(memoryformation.CategoryNotes)
	validContext := item.Context == string(memoryformation.ContextDirectAssertion) ||
		item.Context == string(memoryformation.ContextTemporaryState) ||
		item.Context == string(memoryformation.ContextHypothetical) ||
		item.Context == string(memoryformation.ContextQuotation)
	validProvenance := item.Provenance == string(memoryformation.ProvenanceUserStatement) ||
		item.Provenance == string(memoryformation.ProvenanceModelInference) ||
		item.Provenance == string(memoryformation.ProvenanceThirdParty) ||
		item.Provenance == string(memoryformation.ProvenancePublicSource) ||
		item.Provenance == string(memoryformation.ProvenanceToolOutput)
	validSensitivity := item.Sensitivity == string(memoryformation.SensitivityLow) ||
		item.Sensitivity == string(memoryformation.SensitivityIdentityOrContact) ||
		item.Sensitivity == string(memoryformation.SensitivityHighImpactInteraction)
	return validScope && validCategory && validContext && validProvenance && validSensitivity
}

// DecodeMemorySaveBatchJSON strictly decodes a persisted extraction artifact.
func DecodeMemorySaveBatchJSON(data []byte) (MemorySaveBatch, []MemorySaveItemError, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var arguments map[string]interface{}
	if err := decoder.Decode(&arguments); err != nil {
		return MemorySaveBatch{}, nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return MemorySaveBatch{}, nil, fmt.Errorf("trailing JSON")
	}
	return DecodeMemorySaveBatch(arguments)
}

// SubmitMemorySaveBatch evaluates and atomically applies each item independently.
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
		reason := candidate.DecisionReason
		outcomes = append(outcomes, MemorySaveOutcome{InputIndex: item.InputIndex, CandidateID: candidate.ID, State: candidate.State, PublishedMemoryID: candidate.PublishedMemoryID, Reason: reason})
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
