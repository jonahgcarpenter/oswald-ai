package plugins

import (
	"path/filepath"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

func mustAbs(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		return root
	}
	return abs
}

func effectForChange(root string, oldCfg *config.Config) string {
	// Plugin code changes live outside config.yaml, so the effective config
	// can be identical while a restart is still required. Report it
	// unconditionally: the operator batches installs then runs
	// `oswald restart` once when ready.
	_, _ = root, oldCfg
	return "restart required - run `oswald restart` to apply"
}
