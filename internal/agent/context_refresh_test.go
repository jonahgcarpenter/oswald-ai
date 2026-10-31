package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory/files"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	toolskill "github.com/jonahgcarpenter/oswald-ai/internal/tools/skill_manage"
)

func probeTestAgent(t *testing.T, chat llm.Chatter, reg *registry.Registry) *Agent {
	t.Helper()
	if reg == nil {
		reg = registry.New(config.NewLogger(config.LevelError))
	}
	if err := registerTestTool(t, reg, testToolSpec{Name: "probe", Description: "probe tool"}, testToolPolicy(), func(_ context.Context, _ map[string]interface{}) (governance.Result, error) {
		return productiveResult("probed"), nil
	}); err != nil {
		t.Fatal(err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)
	return agent
}

func TestPromptRefreshesFileMemoryMidTurn(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "probe", nil),
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}},
	}}
	agent := probeTestAgent(t, chat, nil)
	fileRoot := t.TempDir()
	fileStore := files.NewStore(fileRoot)
	agent.SetFileMemory(fileStore)
	if _, err := fileStore.Apply(context.Background(), "user-1", "memory", []files.Operation{{Action: "add", Content: "initial-memory-note"}}); err != nil {
		t.Fatal(err)
	}
	chat.onChat = func(req llm.ChatRequest) {
		if len(chat.requests) == 1 {
			if _, err := fileStore.Apply(context.Background(), "user-1", "memory", []files.Operation{{Action: "add", Content: "midturn-memory-canary"}}); err != nil {
				t.Errorf("apply mid-turn memory: %v", err)
			}
		}
	}
	if _, err := processAgent(agent, "req-refresh-mem", "discord", "session", "user-1", "Display", "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) != 2 {
		t.Fatalf("model calls = %d, want 2", len(requests))
	}
	if system := systemMessageOf(t, requests[0]); strings.Contains(system, "midturn-memory-canary") {
		t.Fatalf("first request already contains mid-turn memory:\n%s", system)
	}
	if system := systemMessageOf(t, requests[1]); !strings.Contains(system, "midturn-memory-canary") {
		t.Fatalf("second request missing refreshed memory:\n%s", system)
	}
	// Snapshot write-through: the same session's next turn keeps the update.
	fixture, ok := agent.userMemory.(*agentMemoryFixture)
	if !ok {
		t.Fatal("test agent has no fixture memory")
	}
	scope, err := fixture.ResolveSessionContext(context.Background(), "user-1", "session", sessionTurnTTL)
	if err != nil {
		t.Fatal(err)
	}
	if user, notes, bound, err := fixture.SessionFileMemory(context.Background(), "user-1", "session", scope.Generation); err != nil || !bound || !strings.Contains(notes, "midturn-memory-canary") {
		t.Fatalf("snapshot missing write-through: bound=%v user=%q notes=%q err=%v", bound, user, notes, err)
	}
	chat2 := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "again"}}}}
	agent.chatClient = chat2
	if _, err := processAgent(agent, "req-refresh-mem-2", "discord", "session", "user-1", "Display", "hello again", nil, nil); err != nil {
		t.Fatal(err)
	}
	if system := systemMessageOf(t, primaryRequests(chat2.requests)[0]); !strings.Contains(system, "midturn-memory-canary") {
		t.Fatalf("next turn lost refreshed memory:\n%s", system)
	}
}

func TestPromptRefreshesSkillsMidTurn(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "probe", nil),
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	if err := reg.RegisterDefinition(toolskill.Definition()); err != nil {
		t.Fatal(err)
	}
	root := skillProfileRoot(t)
	if err := reg.RegisterHandler(toolskill.Name, governance.ToolPolicy{History: governance.HistoryPolicy{Mode: governance.HistoryMetadata, SearchResult: false}}, registry.Handler(toolskill.NewHandler(root))); err != nil {
		t.Fatal(err)
	}
	agent := probeTestAgent(t, chat, reg)
	agent.SetWorkspace(root)
	chat.onChat = func(req llm.ChatRequest) {
		if len(chat.requests) == 1 {
			dir := filepath.Join(root, "skills", "ship")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Errorf("mkdir skill: %v", err)
				return
			}
			body := "---\nname: ship\ndescription: Ship fast\n---\n\nShip it.\n"
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0600); err != nil {
				t.Errorf("write skill: %v", err)
			}
		}
	}
	if _, err := processAgent(agent, "req-refresh-skill", "discord", "session", "user-1", "Display", "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) != 2 {
		t.Fatalf("model calls = %d, want 2", len(requests))
	}
	if system := systemMessageOf(t, requests[1]); !strings.Contains(system, "- ship: Ship fast") {
		t.Fatalf("second request missing refreshed skill index:\n%s", system)
	}
}

func TestPromptRefreshesSoulMidTurn(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		toolCallResponse("call-1", "probe", nil),
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}},
	}}
	agent, fixture := newTestAgent(t, chat, nil, nil)
	if err := registerTestTool(t, agent.registry, testToolSpec{Name: "probe", Description: "probe tool"}, testToolPolicy(), func(_ context.Context, _ map[string]interface{}) (governance.Result, error) {
		return productiveResult("probed"), nil
	}); err != nil {
		t.Fatal(err)
	}
	profileSoul := filepath.Join(fixture.root, "user-1", "SOUL.md")
	chat.onChat = func(req llm.ChatRequest) {
		if len(chat.requests) == 1 {
			if err := os.WriteFile(profileSoul, []byte("midturn-soul-canary directive"), 0600); err != nil {
				t.Errorf("write soul: %v", err)
			}
		}
	}
	if _, err := processAgent(agent, "req-refresh-soul", "discord", "session", "user-1", "Display", "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) != 2 {
		t.Fatalf("model calls = %d, want 2", len(requests))
	}
	if system := systemMessageOf(t, requests[1]); !strings.Contains(system, "midturn-soul-canary") {
		t.Fatalf("second request missing refreshed soul:\n%s", system)
	}
}
