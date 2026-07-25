// Package formationruntime runs durable post-turn memory extraction.
package formationruntime

import (
	"context"

	"github.com/jonahgcarpenter/oswald-ai/internal/memoryextractor"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

var errPermanentExtraction = memoryextractor.ErrPermanentExtraction

// Extractor proposes a shared memory-save batch from one completed turn.
type Extractor interface {
	Extract(context.Context, usermemory.StoredSessionTurn) (usermemory.MemorySaveBatch, error)
}
