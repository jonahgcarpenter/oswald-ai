// Package output sanitizes model-visible command output: ANSI stripping
// and fixed-shape secret redaction. It never logs; callers keep the
// original only in truncated local buffers for debugging without payloads.
package output

import (
	"regexp"
	"strings"
)

var ansi = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

var secretPatterns = []*regexp.Regexp{
	// Bearer tokens and long opaque secrets.
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9\-._~+/=]{16,}`),
	regexp.MustCompile(`(?i)\b(api[_-]?key|secret|token|password)\b\s*[:=]\s*['"]?([A-Za-z0-9\-._~+/=]{12,})['"]?`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9]{16,}\b`),
	regexp.MustCompile(`\bghp_[A-Za-z0-9]{16,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{16,}\b`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),
}

// StripANSI removes terminal escape sequences.
func StripANSI(s string) string {
	return ansi.ReplaceAllString(s, "")
}

// RedactSecrets replaces fixed-shape credential material with a marker.
// It is pattern-based, not a secret detector; source review stays mandatory.
func RedactSecrets(s string) string {
	out := s
	for _, re := range secretPatterns {
		out = re.ReplaceAllString(out, "[REDACTED]")
	}
	return out
}

// Clean applies ANSI stripping then secret redaction.
func Clean(s string) string {
	return RedactSecrets(StripANSI(strings.TrimSpace(s)))
}
