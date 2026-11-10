package startup

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

func stopTestBroker(t *testing.T) *broker.Broker {
	t.Helper()
	return broker.NewBroker(nilProcessor{}, 1, config.NewLogger(config.LevelError))
}

type nilProcessor struct{}

func (nilProcessor) Process(context.Context, agent.Request) (*agent.Response, error) {
	return &agent.Response{}, nil
}

type fakeChildCanceller struct {
	profiles []string
	sessions []string
}

func (f *fakeChildCanceller) CancelSession(profile, sessionKey string) int {
	f.profiles = append(f.profiles, profile)
	f.sessions = append(f.sessions, sessionKey)
	return 1
}

func TestStopWithDelegateCancelsSessionChildren(t *testing.T) {
	br := stopTestBroker(t)
	fake := &fakeChildCanceller{}
	wrapper := &stopWithDelegate{broker: br, profile: "alice", delegate: fake}

	report := wrapper.CancelActiveAgentWork("alice", "discord:dm:1")

	if len(fake.profiles) != 1 || fake.profiles[0] != "alice" || fake.sessions[0] != "discord:dm:1" {
		t.Fatalf("delegate cancel = %v %v", fake.profiles, fake.sessions)
	}
	if report.ActiveSignaled != 0 {
		t.Fatalf("unexpected broker report: %+v", report)
	}
}

func TestStopWithDelegateNilManagerIsBrokerPassthrough(t *testing.T) {
	br := stopTestBroker(t)
	wrapper := &stopWithDelegate{broker: br, profile: "alice"}
	report := wrapper.CancelActiveAgentWork("alice", "discord:dm:1")
	if report.ActiveSignaled != 0 {
		t.Fatalf("unexpected broker report: %+v", report)
	}
}
