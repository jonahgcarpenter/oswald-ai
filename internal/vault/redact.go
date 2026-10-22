// Secret redaction boundary for vault-resolved values.
//
// Resolved passwords and one-time codes are registered here before they
// touch a page. Later browser output (snapshots, console text, eval
// results) passes through ScrubRegistered, so a page that echoes a filled
// secret cannot leak it back to the model.
package vault

import "strings"
import "sync"

const (
	maxRegisteredValues = 64
	maxRegisteredLen    = 1024
	minScrubLen         = 4
)

var registry = struct {
	sync.Mutex
	values []string
}{}

// RegisterSecretValues records secret bytes for later scrubbing. Values
// shorter than minScrubLen are skipped: scrubbing short strings would
// destroy unrelated output. Registration is best-effort and never fails.
func RegisterSecretValues(values ...string) {
	seen := map[string]bool{}
	registry.Lock()
	defer registry.Unlock()
	for _, v := range values {
		if len(v) < minScrubLen || len(v) > maxRegisteredLen || seen[v] {
			continue
		}
		seen[v] = true
		known := false
		for _, existing := range registry.values {
			if existing == v {
				known = true
				break
			}
		}
		if !known {
			registry.values = append(registry.values, v)
			if len(registry.values) > maxRegisteredValues {
				registry.values = registry.values[len(registry.values)-maxRegisteredValues:]
			}
		}
	}
}

// ScrubRegistered replaces every registered secret value with a marker.
func ScrubRegistered(text string) string {
	registry.Lock()
	values := append([]string(nil), registry.values...)
	registry.Unlock()
	for _, v := range values {
		if strings.Contains(text, v) {
			text = strings.ReplaceAll(text, v, "[REDACTED]")
		}
	}
	return text
}

// ScrubValues strips explicit secret values from a string (for example an
// error message) before it can be surfaced.
func ScrubValues(text string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	return text
}
