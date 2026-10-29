package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	toolskill "github.com/jonahgcarpenter/oswald-ai/internal/tools/skill_manage"
)

func skillProfileRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "skills", "review")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: review\ndescription: Review code\n---\n\nFollow the checklist.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func skillTestAgent(t *testing.T, chat llm.Chatter, root string, withTool bool) *Agent {
	t.Helper()
	reg := registry.New(config.NewLogger(config.LevelError))
	if withTool {
		if err := reg.RegisterDefinition(toolskill.Definition()); err != nil {
			t.Fatal(err)
		}
		if err := reg.RegisterHandler(toolskill.Name, governance.ToolPolicy{History: governance.HistoryPolicy{Mode: governance.HistoryMetadata, SearchResult: false}}, registry.Handler(toolskill.NewHandler(root, ""))); err != nil {
			t.Fatal(err)
		}
	}
	agent, _ := newTestAgent(t, chat, nil, reg)
	agent.SetWorkspace(root)
	return agent
}

func systemMessageOf(t *testing.T, req llm.ChatRequest) string {
	t.Helper()
	for _, message := range req.Messages {
		if message.Role == "system" {
			return message.Content
		}
	}
	t.Fatal("no system message in request")
	return ""
}

func TestProcessInjectsSkillsIndexWhenOffered(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
	agent := skillTestAgent(t, chat, skillProfileRoot(t), true)
	if _, err := processAgent(agent, "req-skills", "discord", "session", "user-1", "Display", "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	request := primaryRequests(chat.requests)[0]
	if !requestHasTool(request, toolskill.Name) {
		t.Fatal("skill_manage not offered")
	}
	system := systemMessageOf(t, request)
	for _, want := range []string{"<available_skills>", "- review: Review code"} {
		if !strings.Contains(system, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, system)
		}
	}
	if strings.Contains(system, "Follow the checklist.") {
		t.Fatal("skill body leaked into the index")
	}
}

func TestProcessOmitsSkillsIndexWithoutTool(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
	agent := skillTestAgent(t, chat, skillProfileRoot(t), false)
	if _, err := processAgent(agent, "req-no-skills", "discord", "session", "user-1", "Display", "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	if system := systemMessageOf(t, primaryRequests(chat.requests)[0]); strings.Contains(system, "<available_skills>") {
		t.Fatalf("index without offered tool:\n%s", system)
	}
}

func TestProcessOmitsSkillsIndexForRestrictedToolset(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
	agent := skillTestAgent(t, chat, skillProfileRoot(t), true)
	_, err := agent.Process(context.Background(), Request{
		Principal:   identity.Principal{CanonicalUserID: "user-1", Gateway: "discord", ExternalID: "user-1", Assurance: identity.AssuranceDiscordGateway},
		DisplayName: "Display", SessionKey: "session", Prompt: "hello",
		EnabledToolsets: []string{"terminal"},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := primaryRequests(chat.requests)[0]
	if requestHasTool(request, toolskill.Name) {
		t.Fatal("skill_manage offered under a restricted toolset")
	}
	if system := systemMessageOf(t, request); strings.Contains(system, "<available_skills>") {
		t.Fatalf("index under restricted toolset:\n%s", system)
	}
}
