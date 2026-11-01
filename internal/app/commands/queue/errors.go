package queue

import (
	"context"
	"errors"
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
)

func errUnavailable() error { return fmt.Errorf("queue command is unavailable") }

func queueReason(err error) string {
	switch {
	case errors.Is(err, broker.ErrQueueFull):
		return "queue_full"
	case errors.Is(err, broker.ErrShuttingDown):
		return "shutting_down"
	case errors.Is(err, broker.ErrAgentWorkCanceled):
		return "request_canceled"
	case errors.Is(err, context.Canceled):
		return "request_canceled"
	default:
		return "operation_failed"
	}
}
