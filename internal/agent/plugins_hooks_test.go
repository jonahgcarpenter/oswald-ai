package agent

import (
	"context"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/plugins"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/exposure"
)

type fakeHookInvoker struct {
	preErr    error
	preCalls  int
	postCalls int
}

func (f *fakeHookInvoker) InvokeHook(_ context.Context, name string, _ map[string]any, _ time.Duration) error {
	switch name {
	case plugins.HookPreToolCall:
		f.preCalls++
		return f.preErr
	case plugins.HookPostToolCall:
		f.postCalls++
		return nil
	case plugins.HookSessionStart, plugins.HookSessionEnd:
		return nil
	default:
		return nil
	}
}

func TestExecuteToolPreHookBlocksFailClosed(t *testing.T) {
	chat := &fakeChatter{}
	a, _ := newTestAgent(t, chat, nil, nil)
	a.SetPluginHooks(&fakeHookInvoker{preErr: context.DeadlineExceeded})
	result, err := a.executeTool(context.Background(), identity.Principal{}, "demo_tool", map[string]any{}, exposure.NewExposure(), nil)
	if err != nil {
		t.Fatalf("blocked hook should return result, got err %v", err)
	}
	if result.ReasonCode != "plugin_blocked" {
		t.Fatalf("got %+v", result)
	}
}

func TestExecuteToolPostHookFailOpen(t *testing.T) {
	chat := &fakeChatter{}
	a, _ := newTestAgent(t, chat, nil, nil)
	hooks := &fakeHookInvoker{}
	a.SetPluginHooks(hooks)
	// Unknown tool fails via registry, but post hook still runs fail-open.
	_, _ = a.executeTool(context.Background(), identity.Principal{}, "missing_tool", map[string]any{}, exposure.NewExposure(), nil)
	if hooks.postCalls != 1 {
		t.Fatalf("post calls = %d", hooks.postCalls)
	}
}
