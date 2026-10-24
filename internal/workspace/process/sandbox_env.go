package process

import (
	"os"
	"strings"
)

// GitHubTokenEnv returns the allowlisted GitHub CLI credential entries for
// sandboxed shell environments. Only GH_TOKEN and GITHUB_TOKEN pass through;
// every other ambient secret stays sealed. Values are never logged.
func GitHubTokenEnv() []string {
	var env []string
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// sandboxHome resolves the service HOME for background shells.
func sandboxHome() string {
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return home
	}
	return "/"
}
