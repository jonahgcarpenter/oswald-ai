package memoryformation

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEvaluateActiveConfidenceFloor(t *testing.T) {
	tests := []struct {
		name         string
		confidence   float64
		wantDecision PolicyDecision
		wantApproval Approval
	}{
		{name: "below boundary", confidence: 0.349999, wantDecision: DecisionProposed, wantApproval: ApprovalProposed},
		{name: "at boundary", confidence: 0.35, wantDecision: DecisionAutomatic, wantApproval: ApprovalApproved},
		{name: "above boundary", confidence: 0.350001, wantDecision: DecisionAutomatic, wantApproval: ApprovalApproved},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validCandidate()
			in.Confidence = tt.confidence
			got, err := Evaluate(in)
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if got.Decision != tt.wantDecision || got.Approval != tt.wantApproval {
				t.Fatalf("Evaluate() decision/approval = %s/%s, want %s/%s; reason=%q", got.Decision, got.Approval, tt.wantDecision, tt.wantApproval, got.Reason)
			}
		})
	}
}

func TestEvaluateWholeTurnDirectAndModelInferenceApprove(t *testing.T) {
	tests := []struct {
		name          string
		provenance    Provenance
		statement     string
		wantDecision  PolicyDecision
		wantAuthority SourceAuthority
	}{
		{
			name:          "direct statement",
			provenance:    ProvenanceUserStatement,
			statement:     "The user prefers dark mode.",
			wantDecision:  DecisionAutomatic,
			wantAuthority: AuthorityUserDirect,
		},
		{
			name:          "model inference without lexical overlap",
			provenance:    ProvenanceModelInference,
			statement:     "Visual settings should reduce emitted light.",
			wantDecision:  DecisionInferredActive,
			wantAuthority: AuthorityModel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validCandidate()
			in.Provenance = tt.provenance
			in.Statement = tt.statement
			got, err := Evaluate(in)
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if got.Decision != tt.wantDecision || got.Approval != ApprovalApproved {
				t.Fatalf("Evaluate() decision/approval = %s/%s, want %s/%s; output=%+v", got.Decision, got.Approval, tt.wantDecision, ApprovalApproved, got)
			}
			if got.SourceAuthority != tt.wantAuthority {
				t.Errorf("Evaluate() authority = %q, want %q", got.SourceAuthority, tt.wantAuthority)
			}
		})
	}
}

func TestEvaluateModelInferenceRequiresExactWholeTurnEvidence(t *testing.T) {
	tests := []struct {
		name         string
		source       string
		evidence     string
		wantDecision PolicyDecision
	}{
		{name: "exact whole turn", source: "Dark themes help me focus.", evidence: "Dark themes help me focus.", wantDecision: DecisionInferredActive},
		{name: "exact partial turn", source: "Dark themes help me focus. Use that for this app.", evidence: "Dark themes help me focus.", wantDecision: DecisionProposed},
		{name: "not an exact quote", source: "Dark themes help me focus.", evidence: "Dark themes improve my focus.", wantDecision: DecisionDisallowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validCandidate()
			in.SourceUserText = tt.source
			in.Evidence = tt.evidence
			in.Statement = "The interface should use a low-luminance palette."
			in.Provenance = ProvenanceModelInference
			got, err := Evaluate(in)
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if got.Decision != tt.wantDecision {
				t.Fatalf("Evaluate() decision = %s, want %s; output=%+v", got.Decision, tt.wantDecision, got)
			}
		})
	}
}

func TestEvaluateAllSensitivitiesRetainClassificationAndActivate(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*CandidateInput)
		want         Sensitivity
		wantDecision PolicyDecision
	}{
		{name: "low", want: SensitivityLow, wantDecision: DecisionAutomatic},
		{name: "identity input retained", mutate: func(in *CandidateInput) {
			in.Sensitivity = SensitivityIdentityOrContact
		}, want: SensitivityIdentityOrContact, wantDecision: DecisionAutomatic},
		{name: "identity classification retained", mutate: func(in *CandidateInput) {
			in.SourceUserText = "My name is Alice."
			in.Evidence = in.SourceUserText
			in.Statement = "The user has name Alice."
			in.Category = CategoryIdentity
		}, want: SensitivityIdentityOrContact, wantDecision: DecisionAutomatic},
		{name: "high impact input retained", mutate: func(in *CandidateInput) {
			in.Sensitivity = SensitivityHighImpactInteraction
		}, want: SensitivityHighImpactInteraction, wantDecision: DecisionAutomatic},
		{name: "high impact classification retained", mutate: func(in *CandidateInput) {
			in.SourceUserText = "I never want you to question me."
			in.Evidence = in.SourceUserText
			in.Statement = "The user never wants you to question them."
		}, want: SensitivityHighImpactInteraction, wantDecision: DecisionAutomatic},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validCandidate()
			if tt.mutate != nil {
				tt.mutate(&in)
			}
			got, err := Evaluate(in)
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if got.Sensitivity != tt.want {
				t.Errorf("Evaluate() sensitivity = %q, want %q", got.Sensitivity, tt.want)
			}
			if got.Decision != tt.wantDecision || got.Approval != ApprovalApproved {
				t.Fatalf("Evaluate() decision/approval = %s/%s, want %s/%s; output=%+v", got.Decision, got.Approval, tt.wantDecision, ApprovalApproved, got)
			}
		})
	}
}

func TestEvaluateExplicitRememberApproves(t *testing.T) {
	in := validCandidate()
	in.SourceUserText = "Please remember that my phone is 555-0100"
	in.Statement = "The user's phone number is 555-0100."
	in.Evidence = "my phone is 555-0100"
	in.Category = CategoryIdentity
	in.Sensitivity = SensitivityIdentityOrContact
	in.Mode = ModeExplicitRemember
	in.Confidence = 0

	got, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if got.Decision != DecisionAutomatic || got.Approval != ApprovalApproved {
		t.Fatalf("Evaluate() decision/approval = %s/%s, want %s/%s; output=%+v", got.Decision, got.Approval, DecisionAutomatic, ApprovalApproved, got)
	}
}

func TestEvaluateDisallowedSourcesAndContexts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CandidateInput)
	}{
		{name: "third party provenance", mutate: func(in *CandidateInput) { in.Provenance = ProvenanceThirdParty }},
		{name: "public provenance", mutate: func(in *CandidateInput) { in.Provenance = ProvenancePublicSource }},
		{name: "tool provenance", mutate: func(in *CandidateInput) { in.Provenance = ProvenanceToolOutput }},
		{name: "quoted", mutate: func(in *CandidateInput) { in.Context = ContextQuotation }},
		{name: "hypothetical context", mutate: func(in *CandidateInput) { in.Context = ContextHypothetical }},
		{name: "hypothetical source", mutate: func(in *CandidateInput) {
			in.SourceUserText = "If I move to Paris, I prefer dark mode."
			in.Evidence = in.SourceUserText
		}},
		{name: "third party subject", mutate: func(in *CandidateInput) {
			in.SourceUserText = "My colleague prefers dark mode."
			in.Evidence = in.SourceUserText
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validCandidate()
			tt.mutate(&in)
			got, err := Evaluate(in)
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if got.Decision != DecisionDisallowed || got.Approval != ApprovalProposed {
				t.Fatalf("Evaluate() decision/approval = %s/%s, want %s/%s; output=%+v", got.Decision, got.Approval, DecisionDisallowed, ApprovalProposed, got)
			}
		})
	}
}

func TestEvaluatePartialDirectTurnActivatesButInferenceAndPreCompactionRemainProposed(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*CandidateInput)
		wantDecision PolicyDecision
		wantApproval Approval
	}{
		{name: "partial direct turn", mutate: func(in *CandidateInput) {
			in.SourceUserText = "I prefer dark mode. Please update this screen."
		}, wantDecision: DecisionAutomatic, wantApproval: ApprovalApproved},
		{name: "partial inferred turn", mutate: func(in *CandidateInput) {
			in.SourceUserText = "I prefer dark mode. Please update this screen."
			in.Statement = "The interface should use a low-luminance palette."
			in.Provenance = ProvenanceModelInference
		}, wantDecision: DecisionProposed, wantApproval: ApprovalProposed},
		{name: "pre-compaction direct", mutate: func(in *CandidateInput) {
			in.Mode = ModePreCompactionExtraction
		}, wantDecision: DecisionProposed, wantApproval: ApprovalProposed},
		{name: "pre-compaction inferred", mutate: func(in *CandidateInput) {
			in.Mode = ModePreCompactionExtraction
			in.Statement = "The interface should use a low-luminance palette."
			in.Provenance = ProvenanceModelInference
		}, wantDecision: DecisionProposed, wantApproval: ApprovalProposed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validCandidate()
			tt.mutate(&in)
			got, err := Evaluate(in)
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if got.Decision != tt.wantDecision || got.Approval != tt.wantApproval {
				t.Fatalf("Evaluate() decision/approval = %s/%s, want %s/%s; output=%+v", got.Decision, got.Approval, tt.wantDecision, tt.wantApproval, got)
			}
		})
	}
}

func TestEvaluateDirectCreatorPhraseAndIdentityImportance(t *testing.T) {
	in := validCandidate()
	in.SourceUserText = "For future context, I am your creator. Please answer the question."
	in.Evidence = "I am your creator."
	in.Statement = "The user created Oswald."
	in.Category = CategoryIdentity
	in.Importance = 1

	got, err := Evaluate(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != DecisionAutomatic || got.Approval != ApprovalApproved || got.Importance != 3 {
		t.Fatalf("creator identity output=%+v", got)
	}
}

func TestEvaluateRejectsGenericPartialEvidenceWithoutFirstPerson(t *testing.T) {
	in := validCandidate()
	in.SourceUserText = "Prefers tea. Please update the profile."
	in.Evidence = "Prefers tea."
	in.Statement = "The user prefers tea."

	got, err := Evaluate(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != DecisionDisallowed || got.Approval != ApprovalProposed {
		t.Fatalf("generic evidence was accepted: %+v", got)
	}
}

func TestEvaluateClaimIdentityNormalization(t *testing.T) {
	tests := []struct {
		name      string
		slot      string
		value     string
		statement string
		wantSlot  string
		wantValue string
	}{
		{name: "slot and value", slot: " Environment.Linux Distribution ", value: "Fedora Linux", wantSlot: "environment.linux_distribution", wantValue: "fedora_linux"},
		{name: "defaults", statement: "The user prefers Dark Mode!", wantSlot: "durable_preferences.fact", wantValue: "the_user_prefers_dark_mode"},
		{name: "arch", slot: "environment.linux_distribution", value: "Arch", wantSlot: "environment.linux_distribution", wantValue: "arch_family"},
		{name: "arch linux", slot: "environment.linux_distribution", value: "Arch Linux", wantSlot: "environment.linux_distribution", wantValue: "arch_family"},
		{name: "archlinux", slot: "environment.linux_distribution", value: "ArchLinux", wantSlot: "environment.linux_distribution", wantValue: "arch_family"},
		{name: "arch family", slot: "environment.linux_distribution", value: "arch-family", wantSlot: "environment.linux_distribution", wantValue: "arch_family"},
		{name: "pacman based", slot: "environment.linux_distribution", value: "pacman based", wantSlot: "environment.linux_distribution", wantValue: "arch_family"},
		{name: "pacman based linux", slot: "environment.linux_distribution", value: "pacman-based Linux", wantSlot: "environment.linux_distribution", wantValue: "arch_family"},
		{name: "os family alias", slot: "environment.os_family", value: "Arch Linux", wantSlot: "environment.os_family", wantValue: "arch_family"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validCandidate()
			in.ClaimSlot = tt.slot
			in.ClaimValue = tt.value
			if tt.statement != "" {
				in.Statement = tt.statement
			}
			got, err := Evaluate(in)
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if got.ClaimSlot != tt.wantSlot || got.ClaimValue != tt.wantValue {
				t.Fatalf("Evaluate() claim slot/value = %q/%q, want %q/%q", got.ClaimSlot, got.ClaimValue, tt.wantSlot, tt.wantValue)
			}
			wantKey := tt.wantSlot + "=" + tt.wantValue
			if got.ClaimKey != wantKey {
				t.Errorf("Evaluate() claim key = %q, want %q", got.ClaimKey, wantKey)
			}
		})
	}
}

func TestEvaluateTemporaryState(t *testing.T) {
	tests := []struct {
		name    string
		ttl     time.Duration
		wantTTL time.Duration
	}{
		{name: "default ttl", wantTTL: defaultTaskTTL},
		{name: "custom ttl", ttl: 6 * time.Hour, wantTTL: 6 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validCandidate()
			in.Context = ContextTemporaryState
			in.Scope = ScopeShortTerm
			in.TTL = tt.ttl
			got, err := Evaluate(in)
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if got.Decision != DecisionShortTerm || got.Approval != ApprovalApproved || got.TTL != tt.wantTTL {
				t.Fatalf("Evaluate() = decision %s, approval %s, TTL %s; want %s, %s, %s", got.Decision, got.Approval, got.TTL, DecisionShortTerm, ApprovalApproved, tt.wantTTL)
			}
		})
	}
}

func TestEvaluateRejectsSemanticallyUngroundedDirectStatement(t *testing.T) {
	in := validCandidate()
	in.SourceUserText = "I was thinking aloud"
	in.Evidence = "I"
	in.Statement = "The user prefers tea."
	got, err := Evaluate(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != DecisionDisallowed || got.Approval == ApprovalApproved {
		t.Fatalf("ungrounded candidate was accepted: %+v", got)
	}
}

func TestEvaluateNormalizesEvidenceAndOutput(t *testing.T) {
	in := validCandidate()
	in.SourceUserText = "  I\tprefer   dark mode.  "
	in.Evidence = "I prefer\n dark mode."
	in.Statement = "  The user   prefers dark mode. "

	got, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if got.Statement != `The user directly stated: "I prefer dark mode."` || got.Evidence != "I prefer dark mode." {
		t.Fatalf("Evaluate() normalized output = %q / %q", got.Statement, got.Evidence)
	}
}

func TestEvaluateRejectsInvalidFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CandidateInput)
	}{
		{name: "invalid source UTF-8", mutate: func(in *CandidateInput) { in.SourceUserText = string([]byte{0xff}) }},
		{name: "empty statement", mutate: func(in *CandidateInput) { in.Statement = " \t " }},
		{name: "statement too long", mutate: func(in *CandidateInput) { in.Statement = strings.Repeat("x", maxStatementRunes+1) }},
		{name: "evidence too long", mutate: func(in *CandidateInput) { in.Evidence = strings.Repeat("x", maxEvidenceRunes+1) }},
		{name: "claim slot too long", mutate: func(in *CandidateInput) { in.ClaimSlot = strings.Repeat("x", maxClaimSlotRunes+1) }},
		{name: "claim value too long", mutate: func(in *CandidateInput) { in.ClaimValue = strings.Repeat("x", maxClaimValueRunes+1) }},
		{name: "control", mutate: func(in *CandidateInput) { in.Statement = "bad\x00fact" }},
		{name: "bidi override", mutate: func(in *CandidateInput) { in.Evidence = "I prefer \u202edark mode." }},
		{name: "unknown provenance", mutate: func(in *CandidateInput) { in.Provenance = "unknown" }},
		{name: "unknown claimed authority", mutate: func(in *CandidateInput) { in.ClaimedAuthority = "owner" }},
		{name: "unknown sensitivity", mutate: func(in *CandidateInput) { in.Sensitivity = "none" }},
		{name: "unknown mode", mutate: func(in *CandidateInput) { in.Mode = "manual" }},
		{name: "unknown scope", mutate: func(in *CandidateInput) { in.Scope = "forever" }},
		{name: "unknown category", mutate: func(in *CandidateInput) { in.Category = "secret" }},
		{name: "unknown context", mutate: func(in *CandidateInput) { in.Context = "asserted_by_model" }},
		{name: "negative confidence", mutate: func(in *CandidateInput) { in.Confidence = -0.1 }},
		{name: "nan confidence", mutate: func(in *CandidateInput) { in.Confidence = math.NaN() }},
		{name: "importance zero", mutate: func(in *CandidateInput) { in.Importance = 0 }},
		{name: "importance high", mutate: func(in *CandidateInput) { in.Importance = 6 }},
		{name: "temporary long term", mutate: func(in *CandidateInput) { in.Context = ContextTemporaryState }},
		{name: "temporary ttl too short", mutate: func(in *CandidateInput) {
			in.Context, in.Scope, in.TTL = ContextTemporaryState, ScopeShortTerm, time.Minute
		}},
		{name: "temporary ttl too long", mutate: func(in *CandidateInput) {
			in.Context, in.Scope, in.TTL = ContextTemporaryState, ScopeShortTerm, maxTaskTTL+time.Hour
		}},
		{name: "durable ttl", mutate: func(in *CandidateInput) { in.TTL = time.Hour }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validCandidate()
			tt.mutate(&in)
			if _, err := Evaluate(in); err == nil {
				t.Fatal("Evaluate() error = nil, want validation error")
			}
		})
	}
}

func TestExplicitIntentParsing(t *testing.T) {
	tests := []struct {
		input string
		want  string
		ok    bool
	}{
		{input: "Remember that I prefer dark mode", want: "I prefer dark mode", ok: true},
		{input: " please   remember that I use Go ", want: "I use Go", ok: true},
		{input: "remember this: my timezone is UTC", want: "my timezone is UTC", ok: true},
		{input: "please remember: call me Jo", want: "call me Jo", ok: true},
		{input: "remember I like tea", want: "I like tea", ok: true},
		{input: "could you remember that I like tea", ok: false},
		{input: "remember that", ok: false},
	}
	for _, tt := range tests {
		got, ok := ParseExplicitRemember(tt.input)
		if got != tt.want || ok != tt.ok {
			t.Errorf("ParseExplicitRemember(%q) = %q, %v; want %q, %v", tt.input, got, ok, tt.want, tt.ok)
		}
	}
}

func TestEvaluateDeterministic(t *testing.T) {
	in := validCandidate()
	want, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	for i := 0; i < 100; i++ {
		got, err := Evaluate(in)
		if err != nil {
			t.Fatalf("Evaluate() iteration %d error = %v", i, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Evaluate() iteration %d = %+v, want %+v", i, got, want)
		}
	}
}

func validCandidate() CandidateInput {
	return CandidateInput{
		SourceUserText:   "I prefer dark mode.",
		Statement:        "The user prefers dark mode.",
		Evidence:         "I prefer dark mode.",
		Provenance:       ProvenanceUserStatement,
		ClaimedAuthority: AuthorityUserDirect,
		Sensitivity:      SensitivityLow,
		Mode:             ModeAutomaticExtraction,
		Scope:            ScopeLongTerm,
		Category:         CategoryDurablePreferences,
		Context:          ContextDirectAssertion,
		Confidence:       0.95,
		Importance:       4,
	}
}
