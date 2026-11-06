// Package output sanitizes model-visible command output: ANSI stripping,
// fixed-shape secret redaction, and install-local timestamp rendering. It
// never logs; callers keep the original only in truncated local buffers for
// debugging without payloads.
package output

import (
	"regexp"
	"strings"
	"time"
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

// FormatModelTime renders an instant for model consumption in the install's
// local zone with an explicit zone suffix, so the label never reads as a
// different zone's wall clock. The install's zone comes from the OS
// (time.Local honors TZ); no zone is hardcoded here. In UTC installs the
// suffix reads "UTC".
func FormatModelTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(time.Local).Format("Monday, January 02, 2006 at 03:04 PM MST (UTC-07:00)")
}

// FormatModelTimestamp renders an RFC3339 UTC instant for model consumption,
// appending the same instant in the install's local zone. next must parse as
// RFC3339; unparseable input returns the trimmed input unchanged.
func FormatModelTimestamp(next string) string {
	trimmed := strings.TrimSpace(next)
	if trimmed == "" || trimmed == "-" {
		return trimmed
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return trimmed
	}
	return trimmed + " (" + FormatModelTime(parsed) + ")"
}
