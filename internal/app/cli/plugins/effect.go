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
	if oldCfg != nil {
		if nextCfg, err := config.Validate(root); err == nil {
			if config.ReloadEffect(oldCfg, nextCfg) == "live" {
				return "applied live"
			}
		}
	}
	return "restarting service to apply"
}
