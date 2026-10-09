package start

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/cli/setup"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestStartRunsSeededRootUntilCancelled(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, config.OswaldHomeDir)
	var setupOut, setupErr bytes.Buffer
	if code := setup.Run(context.Background(), root, nil, &setupOut, &setupErr); code != 0 {
		t.Fatalf("seed root = %d: %s", code, setupErr.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := Run(ctx, root, io.Discard); code != 0 {
		t.Fatalf("start on seeded root = %d, want 0", code)
	}
}

func TestStartMissingRootReturnsError(t *testing.T) {
	// The failure diagnostic is logged to stderr by startup.Serve; only the
	// external exit code is asserted here.
	if code := Run(context.Background(), filepath.Join(t.TempDir(), "missing"), io.Discard); code != 1 {
		t.Fatalf("start on missing root = %d, want 1", code)
	}
}
