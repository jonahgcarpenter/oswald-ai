// Package start implements the `oswald start` command: run the configured
// service in the foreground.
package start

import (
	"context"
	"io"

	"github.com/jonahgcarpenter/oswald-ai/internal/startup"
)

// Run starts the service against the selected configuration root and blocks
// until shutdown or startup failure. Diagnostics are logged by startup.Serve.
func Run(ctx context.Context, root string, stdout io.Writer) int {
	if err := startup.Serve(ctx, root, stdout); err != nil {
		return 1
	}
	return 0
}
