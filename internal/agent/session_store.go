package agent

import (
	"context"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
)

// SessionStore is the agent's profile-scoped conversation persistence contract.
type SessionStore interface {
	ResolveSessionContext(context.Context, string, string, time.Duration) (memory.SessionContext, error)
	SessionFileMemory(context.Context, string, string, int) (string, string, bool, error)
	BindSessionFileMemory(context.Context, string, string, int, string, string) (string, string, error)
	UpdateSessionFileMemory(context.Context, string, string, int, string, string) (string, string, error)
	SessionImages(context.Context, string, string, int) ([]requestctx.InputImage, error)
	ReserveImageVersion(context.Context, string, string, int, string, int) (int, error)
	RecentCompletedExchangesAfter(context.Context, string, string, int, int64, int) ([]memory.SessionTurn, error)
	AppendPendingSessionTurn(context.Context, memory.SessionTurnWrite) (memory.StoredSessionTurn, error)
	RecordModelUsage(context.Context, memory.ModelUsageRecord) error
	MarkSessionTurnDelivered(context.Context, string, int64) error
	MarkSessionTurnsCompacted(context.Context, string, string, int, []int64) error
}
