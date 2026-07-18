package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

func TestAssemblePromptContextPreservesRolesAndOrder(t *testing.T) {
	turns := []usermemory.SessionTurn{
		{ID: 3, UserText: "new user", AssistantText: "new assistant", ToolNames: []string{"web.search", "time.current"}},
		{ID: 2, UserText: "middle user", AssistantText: "middle assistant", ToolNames: []string{"web.search"}},
		{ID: 1, UserText: "old user", AssistantText: "old assistant"},
	}

	got := AssemblePromptContext("deployment policy", "tenant profile", "current", nil, turns, nil, 100000)
	wantRoles := []string{"system", "user", "user", "assistant", "user", "assistant", "user", "assistant", "user"}
	wantContents := []string{
		"deployment policy", "tenant profile",
		"old user", "old assistant",
		"middle user", "middle assistant\n\nTools used: web.search",
		"new user", "new assistant\n\nTools used: web.search, time.current",
		"current",
	}
	if roles(got.Messages) != strings.Join(wantRoles, ",") {
		t.Fatalf("roles = %s, want %s", roles(got.Messages), strings.Join(wantRoles, ","))
	}
	for i, message := range got.Messages {
		if message.Content != wantContents[i] {
			t.Fatalf("message %d content = %q, want %q", i, message.Content, wantContents[i])
		}
	}
	if ids(got.SelectedTurns) != "1,2,3" {
		t.Fatalf("selected turn order = %s, want 1,2,3", ids(got.SelectedTurns))
	}
	if !reflect.DeepEqual(got.SelectedToolNames, []string{"web.search", "time.current"}) {
		t.Fatalf("selected tools = %#v", got.SelectedToolNames)
	}
	if got.SelectedTurnCount != 3 || got.OmittedTurnCount != 0 {
		t.Fatalf("unexpected counts: selected=%d omitted=%d", got.SelectedTurnCount, got.OmittedTurnCount)
	}
}

func TestAssemblePromptContextExactFitAndOneTokenOver(t *testing.T) {
	turn := usermemory.SessionTurn{UserText: "historical question", AssistantText: "historical answer"}
	all := AssemblePromptContext("policy", "", "now", nil, []usermemory.SessionTurn{turn}, nil, 100000)

	exact := AssemblePromptContext("policy", "", "now", nil, []usermemory.SessionTurn{turn}, nil, all.EstimatedAfter)
	if exact.SelectedTurnCount != 1 || exact.EstimatedAfter != all.EstimatedAfter {
		t.Fatalf("exact fit rejected: %+v", exact)
	}

	over := AssemblePromptContext("policy", "", "now", nil, []usermemory.SessionTurn{turn}, nil, all.EstimatedAfter-1)
	if over.SelectedTurnCount != 0 || len(over.Messages) != 2 {
		t.Fatalf("one-token over included history: %+v", over)
	}
}

func TestAssemblePromptContextStopsAtOversizedNewestTurn(t *testing.T) {
	turns := []usermemory.SessionTurn{
		{ID: 3, UserText: strings.Repeat("界", 4000), AssistantText: "too large"},
		{ID: 2, UserText: "small", AssistantText: "would fit"},
	}
	required := AssemblePromptContext("policy", "profile", "current", nil, nil, nil, 100000)
	smallOnly := AssemblePromptContext("policy", "profile", "current", nil, turns[1:], nil, 100000)
	limit := smallOnly.EstimatedAfter
	if limit <= required.EstimatedAfter {
		t.Fatal("test setup did not make the older turn fit")
	}

	got := AssemblePromptContext("policy", "profile", "current", nil, turns, nil, limit)
	if got.SelectedTurnCount != 0 || got.OmittedTurnCount != 2 {
		t.Fatalf("assembler skipped past non-fitting newest turn: %+v", got)
	}
	if !strings.Contains(turns[0].UserText, "界") {
		t.Fatal("UTF-8 test fixture was unexpectedly modified")
	}
}

func TestAssemblePromptContextRequiredOverBudgetPreservesRequiredMessages(t *testing.T) {
	images := []llm.InputImage{{MimeType: "image/png", Data: "one"}, {MimeType: "image/jpeg", Data: "two"}}
	turns := []usermemory.SessionTurn{{UserText: "old user", AssistantText: "old assistant"}}
	tools := []llm.Tool{{Type: "function", Function: llm.ToolDefinition{Name: "web.search", Description: strings.Repeat("schema", 100)}}}

	got := AssemblePromptContext("policy", "profile", "current", images, turns, tools, 1)
	if !got.RequiredOverBudget || got.SelectedTurnCount != 0 || got.OmittedTurnCount != 1 {
		t.Fatalf("unexpected over-budget result: %+v", got)
	}
	if len(got.Messages) != 3 || got.Messages[0].Role != "system" || got.Messages[1].Role != "user" || got.Messages[2].Role != "user" {
		t.Fatalf("required messages not preserved: %#v", got.Messages)
	}
	if len(got.Messages[2].Images) != 2 || len(got.Messages[0].Images) != 0 || len(got.Messages[1].Images) != 0 {
		t.Fatalf("images must remain current-turn-only: %#v", got.Messages)
	}
	if got.EstimatedAfter != got.RequiredEstimate || got.EstimatedBefore <= got.EstimatedAfter {
		t.Fatalf("unexpected estimates: before=%d required=%d after=%d", got.EstimatedBefore, got.RequiredEstimate, got.EstimatedAfter)
	}
}

func TestAssemblePromptContextToolsAffectSelectionBudget(t *testing.T) {
	turn := usermemory.SessionTurn{UserText: strings.Repeat("u", 100), AssistantText: strings.Repeat("a", 100)}
	withoutTools := AssemblePromptContext("policy", "", "current", nil, []usermemory.SessionTurn{turn}, nil, 100000)
	tools := []llm.Tool{{Type: "function", Function: llm.ToolDefinition{Name: "large.tool", Description: strings.Repeat("description", 200)}}}

	got := AssemblePromptContext("policy", "", "current", nil, []usermemory.SessionTurn{turn}, tools, withoutTools.EstimatedAfter)
	if got.SelectedTurnCount != 0 {
		t.Fatalf("tool schema was not included in the selection estimate: %+v", got)
	}
	if got.RequiredEstimate != promptbudget.EstimateRequest(got.Messages, tools) {
		t.Fatalf("required estimate does not include tools: got %d", got.RequiredEstimate)
	}
}

func roles(messages []llm.ChatMessage) string {
	values := make([]string, len(messages))
	for i, message := range messages {
		values[i] = message.Role
	}
	return strings.Join(values, ",")
}

func ids(turns []usermemory.SessionTurn) string {
	values := make([]string, len(turns))
	for i, turn := range turns {
		values[i] = string(rune('0' + turn.ID))
	}
	return strings.Join(values, ",")
}
