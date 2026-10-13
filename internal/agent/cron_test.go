package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/files"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/exposure"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/workspace"
)

func cronPrincipal() identity.Principal {
	return identity.Principal{CanonicalUserID: "user-1", Gateway: "cron", ExternalID: "cron:user-1", Assurance: identity.AssuranceCronScheduler}
}

func TestSkipMemoryOmitsFileMemory(t *testing.T) {
	newSkippedAgent := func(t *testing.T, skip bool) (*Agent, *fakeChatter) {
		chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
		reg := registry.New(config.NewLogger(config.LevelError))
		a, fixture := newTestAgent(t, chat, nil, reg)
		memDir := filepath.Join(fixture.root, "user-1", "memories")
		if err := os.MkdirAll(memDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(memDir, "USER.md"), []byte("Cron-only profile note"), 0600); err != nil {
			t.Fatal(err)
		}
		a.SetFileMemory(files.NewProfileStore(filepath.Join(fixture.root, "user-1"), "user-1"))
		_, err := a.Process(context.Background(), Request{
			RequestID: "cron-1", Principal: cronPrincipal(), DisplayName: "Scheduler",
			SessionKey: "cron:job1", Prompt: "go", SkipMemory: skip,
		})
		if err != nil {
			t.Fatal(err)
		}
		return a, chat
	}
	_, skipped := newSkippedAgent(t, true)
	system := skipped.requests[0].Messages[0].Content
	if strings.Contains(system, "Cron-only profile note") {
		t.Fatalf("skip_memory leaked file memory:\n%s", system)
	}
	if !strings.Contains(system, "You are Oswald.") {
		t.Fatal("skip_memory dropped the soul")
	}
	_, kept := newSkippedAgent(t, false)
	if !strings.Contains(kept.requests[0].Messages[0].Content, "Cron-only profile note") {
		t.Fatal("control run missing file memory")
	}
}

func TestToolsForRequestToolsetFilter(t *testing.T) {
	reg := registry.New(config.NewLogger(config.LevelError))
	ok := func(ctx context.Context, _ map[string]interface{}) (governance.Result, error) {
		return productiveResult("ok"), nil
	}
	for _, name := range []string{workspace.ReadFileName, workspace.TerminalName, workspace.ProcessManageName} {
		if err := registerTestTool(t, reg, testToolSpec{Name: name, Description: "Tool " + name}, governance.ToolPolicy{}, ok); err != nil {
			t.Fatal(err)
		}
	}
	chat := &fakeChatter{}
	a, _ := newTestAgent(t, chat, nil, reg)
	ctx := context.Background()
	principal := cronPrincipal()

	full := a.toolsForRequest(ctx, principal, exposure.NewExposure(), nil)
	if len(full.Tools) != 3 {
		t.Fatalf("unfiltered catalog = %d tools", len(full.Tools))
	}
	core := a.toolsForRequest(ctx, principal, exposure.NewExposure(), []string{"core"})
	if len(core.Tools) != 1 || core.Tools[0].Function.Name != workspace.ReadFileName {
		t.Fatalf("core catalog = %v", catalogToolNames(core.Tools))
	}
	empty := a.toolsForRequest(ctx, principal, exposure.NewExposure(), []string{})
	if len(empty.Tools) != 0 {
		t.Fatalf("zero-tool catalog = %v", catalogToolNames(empty.Tools))
	}
}

func TestTerminalHintInjectedOnlyWithProcessManage(t *testing.T) {
	reg := registry.New(config.NewLogger(config.LevelError))
	ok := func(ctx context.Context, _ map[string]interface{}) (governance.Result, error) {
		return productiveResult("ok"), nil
	}
	newCatalog := func(names ...string) offeredToolCatalog {
		r := registry.New(config.NewLogger(config.LevelError))
		for _, name := range names {
			if err := registerTestTool(t, r, testToolSpec{Name: name, Description: "Tool " + name}, governance.ToolPolicy{}, ok); err != nil {
				t.Fatal(err)
			}
		}
		chat := &fakeChatter{}
		a, _ := newTestAgent(t, chat, nil, r)
		_ = reg
		return a.toolsForRequest(context.Background(), cronPrincipal(), exposure.NewExposure(), nil)
	}
	with := newCatalog(workspace.TerminalName, workspace.ProcessManageName)
	for _, tool := range with.Tools {
		if tool.Function.Name == workspace.TerminalName && !strings.Contains(tool.Function.Description, "process_manage") {
			t.Fatal("hint missing when companion advertised")
		}
	}
	without := newCatalog(workspace.TerminalName)
	for _, tool := range without.Tools {
		if tool.Function.Name == workspace.TerminalName && strings.Contains(tool.Function.Description, "process_manage") {
			t.Fatalf("hint leaked without companion: %q", tool.Function.Description)
		}
	}
}

func catalogToolNames(tools []llm.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Function.Name)
	}
	return names
}
