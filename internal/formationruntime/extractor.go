// Package formationruntime runs durable post-turn memory extraction.
package formationruntime

import (
	"context"

	"github.com/jonahgcarpenter/oswald-ai/internal/memoryextractor"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

var errPermanentExtraction = memoryextractor.ErrPermanentExtraction
var errInvalidOutput = memoryextractor.ErrInvalidOutput
var invalidOutputCode = memoryextractor.InvalidOutputCode

// Extractor proposes a shared memory-save batch from one completed turn.
type Extractor interface {
	Extract(context.Context, usermemory.StoredSessionTurn, string) (usermemory.MemorySaveBatch, error)
}

// PatternExtractor proposes repeated signals from a frozen user-turn window.
type PatternExtractor interface {
	ExtractPatterns(context.Context, []usermemory.StoredSessionTurn, string) (usermemory.MemoryPatternBatch, error)
}
