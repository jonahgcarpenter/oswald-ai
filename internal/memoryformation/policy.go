package memoryformation

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxSourceRunes    = 16_000
	maxStatementRunes = 1_000
	maxEvidenceRunes  = 1_000
	defaultTaskTTL    = 24 * time.Hour
	minTaskTTL        = time.Hour
	maxTaskTTL        = 30 * 24 * time.Hour
)

var errInvalidCandidate = errors.New("invalid memory candidate")

// Evaluate validates, normalizes, and deterministically classifies a candidate.
func Evaluate(in CandidateInput) (CandidateOutput, error) {
	if err := validateCandidate(in); err != nil {
		return CandidateOutput{}, err
	}

	out := CandidateOutput{
		Statement:       normalizeText(in.Statement),
		Evidence:        normalizeText(in.Evidence),
		Provenance:      in.Provenance,
		SourceAuthority: authorityFor(in.Provenance),
		Sensitivity:     maxSensitivity(in.Sensitivity, ClassifySensitivity(in.Statement, in.Category)),
		Mode:            in.Mode,
		Scope:           in.Scope,
		Category:        in.Category,
		Context:         in.Context,
		Confidence:      in.Confidence,
		Importance:      in.Importance,
		TTL:             in.TTL,
		Approval:        ApprovalProposed,
		Decision:        DecisionProposed,
		Reason:          "candidate requires review",
	}
	source := normalizeText(in.SourceUserText)

	if !strings.Contains(source, out.Evidence) {
		return disallow(out, "evidence is not an exact quote from normalized source user text"), nil
	}
	if !statementGroundedByEvidence(out.Statement, out.Evidence) {
		return disallow(out, "statement is not meaningfully grounded by its exact evidence quote"), nil
	}
	if in.Context == ContextHypothetical || in.Context == ContextQuotation {
		return disallow(out, "hypothetical and quoted content is not user memory"), nil
	}
	if in.Mode == ModeAutomaticExtraction && isNonAssertiveSource(source) {
		return disallow(out, "automatic source is hypothetical or indirect"), nil
	}
	if containsPromptInjection(source) || containsPromptInjection(out.Statement) || containsPromptInjection(out.Evidence) {
		return disallow(out, "instruction-like content cannot become user memory"), nil
	}
	if in.Provenance == ProvenanceThirdParty || in.Provenance == ProvenancePublicSource || in.Provenance == ProvenanceToolOutput {
		return disallow(out, "external facts cannot become tenant memory"), nil
	}
	if in.Provenance == ProvenanceModelInference {
		out.Reason = "model inferences remain proposed"
		return out, nil
	}
	if in.Provenance == ProvenanceUserStatement && in.Mode == ModeAutomaticExtraction && !hasDirectUserMarker(out.Evidence) {
		return disallow(out, "automatic user-statement evidence lacks a direct first-person marker"), nil
	}
	if in.Provenance == ProvenanceUserStatement && in.Mode == ModeAutomaticExtraction && containsThirdPartySubject(out.Evidence) {
		return disallow(out, "automatic evidence describes a third party"), nil
	}
	if in.Provenance == ProvenanceUserStatement {
		if in.Mode == ModeExplicitRemember {
			out.Statement = fmt.Sprintf("The user explicitly asked to remember: %q", out.Evidence)
		} else {
			out.Statement = fmt.Sprintf("The user directly stated: %q", out.Evidence)
		}
		out.Sensitivity = maxSensitivity(out.Sensitivity, ClassifySensitivity(out.Statement+" "+out.Evidence, out.Category))
	}
	if in.Mode == ModeAutomaticExtraction && source != out.Evidence {
		out.Reason = "partial-turn extraction remains proposed"
		return out, nil
	}

	if in.Mode == ModeExplicitRemember {
		remembered, ok := ParseExplicitRemember(in.SourceUserText)
		if !ok || !strings.Contains(normalizeText(remembered), out.Evidence) {
			return disallow(out, "explicit mode requires an exact remember phrase containing the evidence"), nil
		}
		if in.Context == ContextTemporaryState {
			return approveShortTerm(out), nil
		}
		out.Approval = ApprovalApproved
		out.Decision = DecisionAutomatic
		out.Reason = "user explicitly requested this memory"
		return out, nil
	}

	if out.Sensitivity != SensitivityLow {
		if in.Context == ContextTemporaryState && out.TTL == 0 {
			out.TTL = defaultTaskTTL
		}
		out.Approval = ApprovalPendingConfirmation
		out.Decision = DecisionPendingConfirmation
		out.Reason = "sensitive or high-impact memory requires confirmation"
		return out, nil
	}
	if in.Context == ContextTemporaryState {
		return approveShortTerm(out), nil
	}
	if isAutomaticCategory(in.Category) {
		out.Approval = ApprovalApproved
		out.Decision = DecisionAutomatic
		out.Reason = "directly stated stable low-sensitivity fact"
	}
	return out, nil
}

// ClassifySensitivity derives the minimum sensitivity from canonical content;
// callers may raise but never lower this classification.
func ClassifySensitivity(statement string, category Category) Sensitivity {
	lower := strings.ToLower(normalizeText(statement))
	if category == CategoryIdentity {
		return SensitivityIdentityOrContact
	}
	for _, marker := range []string{"my name", "user's name", "phone", "email", "address", "contact", "social security", "passport", "birthday", "date of birth", "born", "home location", "where i live", "where the user lives"} {
		if strings.Contains(lower, marker) {
			return SensitivityIdentityOrContact
		}
	}
	hasHighImpactDirective := false
	for _, marker := range []string{"always", "never", "must", "without asking", "do not question", "ignore"} {
		if strings.Contains(lower, marker) {
			hasHighImpactDirective = true
			break
		}
	}
	if hasHighImpactDirective {
		for _, marker := range []string{" you ", " reply", " respond", " answer", " question me", " ask me", " talk to me", " call me"} {
			if strings.Contains(" "+lower+" ", marker) {
				return SensitivityHighImpactInteraction
			}
		}
	}
	return SensitivityLow
}

func maxSensitivity(a, b Sensitivity) Sensitivity {
	rank := func(value Sensitivity) int {
		switch value {
		case SensitivityHighImpactInteraction:
			return 3
		case SensitivityIdentityOrContact:
			return 2
		default:
			return 1
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// ParseExplicitRemember recognizes only the supported explicit intent forms and
// returns the text following the phrase. Matching is case-insensitive.
func ParseExplicitRemember(text string) (string, bool) {
	normalized := normalizeText(text)
	lower := strings.ToLower(normalized)
	for _, prefix := range []string{"remember that ", "please remember that ", "remember this: ", "please remember: ", "remember ", "please remember "} {
		if strings.HasPrefix(lower, prefix) {
			value := strings.TrimSpace(normalized[len(prefix):])
			if strings.EqualFold(value, "that") || strings.EqualFold(value, "this") {
				return "", false
			}
			return value, value != ""
		}
	}
	return "", false
}

// ParseConfirmation recognizes a deliberately small set of exact phrases.
// Matching ignores case and surrounding or repeated whitespace, but not punctuation.
func ParseConfirmation(text string) Confirmation {
	switch strings.ToLower(normalizeText(text)) {
	case "yes remember it":
		return ConfirmationYes
	case "no do not save it":
		return ConfirmationNo
	default:
		return ConfirmationUnknown
	}
}

func statementGroundedByEvidence(statement, evidence string) bool {
	statementTokens := formationTokens(statement)
	evidenceTokens := formationTokens(evidence)
	if len(statementTokens) == 0 || len(evidenceTokens) == 0 {
		return false
	}
	matched := 0
	for token := range statementTokens {
		if _, ok := evidenceTokens[token]; ok {
			matched++
		}
	}
	return matched >= 1 && float64(matched)/float64(len(statementTokens)) >= 0.5
}

func formationTokens(value string) map[string]struct{} {
	tokens := make(map[string]struct{})
	for _, field := range strings.FieldsFunc(strings.ToLower(normalizeText(value)), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		field = strings.TrimSuffix(field, "s")
		if len([]rune(field)) < 2 || formationStopWords[field] {
			continue
		}
		tokens[field] = struct{}{}
	}
	return tokens
}

var formationStopWords = map[string]bool{
	"the": true, "user": true, "i": true, "my": true, "me": true, "is": true,
	"am": true, "are": true, "a": true, "an": true, "to": true, "that": true,
}

func hasDirectUserMarker(evidence string) bool {
	lower := " " + strings.ToLower(normalizeText(evidence)) + " "
	for _, marker := range []string{" i ", " i'm ", " i've ", " i'd ", " my ", " me ", " we ", " our "} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func containsThirdPartySubject(evidence string) bool {
	lower := " " + strings.ToLower(normalizeText(evidence)) + " "
	for _, marker := range []string{" my sister ", " my brother ", " my friend ", " my partner ", " my colleague ", " my coworker ", " my mother ", " my father ", " my mom ", " my dad ", " they ", " she ", " he "} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isNonAssertiveSource(source string) bool {
	lower := strings.ToLower(normalizeText(source))
	for _, prefix := range []string{"if ", "suppose ", "supposing ", "imagine ", "hypothetically ", "what if ", "could ", "would "} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	for _, marker := range []string{" i heard ", " i think ", " maybe ", " perhaps "} {
		if strings.Contains(" "+lower+" ", marker) {
			return true
		}
	}
	return false
}

func validateCandidate(in CandidateInput) error {
	fields := []struct {
		name  string
		value string
		max   int
	}{
		{name: "source_user_text", value: in.SourceUserText, max: maxSourceRunes},
		{name: "statement", value: in.Statement, max: maxStatementRunes},
		{name: "evidence", value: in.Evidence, max: maxEvidenceRunes},
	}
	for _, field := range fields {
		name, value := field.name, field.value
		if !utf8.ValidString(value) {
			return invalid(name, "must be valid UTF-8")
		}
		if hasUnsafeRune(value) {
			return invalid(name, "contains a control or bidirectional formatting character")
		}
		if utf8.RuneCountInString(normalizeText(value)) == 0 || utf8.RuneCountInString(value) > field.max {
			return invalid(name, fmt.Sprintf("length must be 1..%d runes", field.max))
		}
	}
	if !validProvenance(in.Provenance) || !validAuthority(in.ClaimedAuthority) || !validSensitivity(in.Sensitivity) ||
		!validMode(in.Mode) || !validScope(in.Scope) || !validCategory(in.Category) || !validContext(in.Context) {
		return invalid("classification", "contains an unknown enum value")
	}
	if math.IsNaN(in.Confidence) || math.IsInf(in.Confidence, 0) || in.Confidence < 0 || in.Confidence > 1 {
		return invalid("confidence", "must be finite and between 0 and 1")
	}
	if in.Importance < 1 || in.Importance > 5 {
		return invalid("importance", "must be between 1 and 5")
	}
	if in.Context == ContextTemporaryState {
		if in.Scope != ScopeShortTerm {
			return invalid("scope", "temporary task state must be short_term")
		}
		if in.TTL != 0 && (in.TTL < minTaskTTL || in.TTL > maxTaskTTL) {
			return invalid("ttl", "temporary TTL must be between 1 hour and 30 days")
		}
	} else {
		if in.Scope != ScopeLongTerm {
			return invalid("scope", "non-temporary memory must be long_term")
		}
		if in.TTL != 0 {
			return invalid("ttl", "only temporary task state may set TTL")
		}
	}
	return nil
}

func normalizeText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func hasUnsafeRune(value string) bool {
	for _, r := range value {
		if (unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t') || isBidiControl(r) {
			return true
		}
	}
	return false
}

func isBidiControl(r rune) bool {
	return r == '\u061c' || r == '\u200e' || r == '\u200f' ||
		(r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069')
}

func containsPromptInjection(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"ignore previous instructions", "ignore all previous instructions",
		"override previous instructions", "system prompt", "developer message",
		"you are now", "<|system|>", "[system]",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func authorityFor(provenance Provenance) SourceAuthority {
	switch provenance {
	case ProvenanceUserStatement:
		return AuthorityUserDirect
	case ProvenanceModelInference:
		return AuthorityModel
	case ProvenanceThirdParty:
		return AuthorityThirdParty
	case ProvenancePublicSource:
		return AuthorityPublic
	default:
		return AuthorityTool
	}
}

func approveShortTerm(out CandidateOutput) CandidateOutput {
	out.Approval = ApprovalApproved
	out.Decision = DecisionShortTerm
	out.Reason = "temporary task state has bounded retention"
	if out.TTL == 0 {
		out.TTL = defaultTaskTTL
	}
	return out
}

func disallow(out CandidateOutput, reason string) CandidateOutput {
	out.Decision = DecisionDisallowed
	out.Reason = reason
	return out
}

func invalid(field, reason string) error {
	return fmt.Errorf("%w: %s %s", errInvalidCandidate, field, reason)
}

func isAutomaticCategory(category Category) bool {
	switch category {
	case CategoryCommunicationPreferences, CategoryDurablePreferences, CategoryProjects, CategoryEnvironment:
		return true
	default:
		return false
	}
}

func validProvenance(v Provenance) bool {
	return v == ProvenanceUserStatement || v == ProvenanceModelInference || v == ProvenanceThirdParty || v == ProvenancePublicSource || v == ProvenanceToolOutput
}

func validAuthority(v SourceAuthority) bool {
	return v == AuthorityUserDirect || v == AuthorityModel || v == AuthorityThirdParty || v == AuthorityPublic || v == AuthorityTool
}

func validSensitivity(v Sensitivity) bool {
	return v == SensitivityLow || v == SensitivityIdentityOrContact || v == SensitivityHighImpactInteraction
}

func validMode(v FormationMode) bool {
	return v == ModeAutomaticExtraction || v == ModeExplicitRemember
}

func validScope(v Scope) bool { return v == ScopeShortTerm || v == ScopeLongTerm }

func validCategory(v Category) bool {
	return v == CategoryIdentity || v == CategoryCommunicationPreferences || v == CategoryDurablePreferences || v == CategoryProjects || v == CategoryRelationships || v == CategoryEnvironment || v == CategoryNotes
}

func validContext(v ContentContext) bool {
	return v == ContextDirectAssertion || v == ContextTemporaryState || v == ContextHypothetical || v == ContextQuotation
}
