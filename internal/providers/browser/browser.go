// Package browser drives a local headless Chromium via CDP for the
// model-facing browser_* tools. All behavior assumes an on-machine browser;
// there are no hosted search/extract services and no cloud browser.
package browser

import "time"

// Options configures the local headless backend. Zero values resolve via
// Effective.
type Options struct {
	// LaunchCommand overrides the Chromium binary lookup (binary path plus
	// optional arguments, space-separated). Empty searches PATH.
	LaunchCommand string
	// EndpointURL overrides launching and dials an existing DevTools HTTP
	// endpoint (e.g. http://127.0.0.1:9222).
	EndpointURL string
	// CommandTimeout bounds one browser command. Floor 5s, default 30s.
	CommandTimeout time.Duration
	// SnapshotThreshold bounds compact snapshots before paging to a file.
	// Floor 1000 runes, default 15000.
	SnapshotThreshold int
	// InactivityTimeout reaps idle sessions. Floor 30s, default 120s.
	InactivityTimeout time.Duration
	// RecordSessions is accepted for config compatibility; session recording
	// has no model-visible control in v1 and is a no-op.
	RecordSessions bool
	// AllowPrivateURLs permits loopback/private/link-local destinations.
	// Default false (fail closed).
	AllowPrivateURLs bool
	// AllowInsecureURLs permits plaintext http page loads. Default false
	// (https only); other schemes stay blocked regardless.
	AllowInsecureURLs bool
}

// Effective returns Options with defaults and floors applied.
func (o Options) Effective() Options {
	if o.CommandTimeout < 5*time.Second {
		if o.CommandTimeout <= 0 {
			o.CommandTimeout = 30 * time.Second
		} else {
			o.CommandTimeout = 5 * time.Second
		}
	}
	if o.SnapshotThreshold < 1000 {
		if o.SnapshotThreshold <= 0 {
			o.SnapshotThreshold = 15000
		} else {
			o.SnapshotThreshold = 1000
		}
	}
	if o.InactivityTimeout < 30*time.Second {
		if o.InactivityTimeout <= 0 {
			o.InactivityTimeout = 120 * time.Second
		} else {
			o.InactivityTimeout = 30 * time.Second
		}
	}
	return o
}

// OpenTimeout returns the navigation timeout: 60s floor, 120s floor on first
// open for cold daemon plus Chromium launch.
func (o Options) OpenTimeout(firstNav bool) time.Duration {
	o = o.Effective()
	if firstNav {
		if o.CommandTimeout < 120*time.Second {
			return 120 * time.Second
		}
		return o.CommandTimeout
	}
	if o.CommandTimeout < 60*time.Second {
		return 60 * time.Second
	}
	return o.CommandTimeout
}
