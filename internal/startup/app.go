package startup

import (
	"context"
	"fmt"
	"io"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// Error preserves a fixed operational event and the initialization cause.
type Error struct {
	Event   string
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

// Unwrap exposes the initialization cause after cleanup has completed.
func (e *Error) Unwrap() error { return e.Cause }

// Run assembles the profile-based application without account or MCP stores.
func Run(ctx context.Context, cfg *config.Config, log *config.Logger, _ io.Writer) error {
	return runProfiles(ctx, cfg, log)
}
