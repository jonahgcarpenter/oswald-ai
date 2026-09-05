package config

import (
	"regexp"
	"strings"
)

// FallbackErrorText is the user-facing message used when no safe error text remains.
const FallbackErrorText = "Something broke, Try again or pitch in."

var (
	ipv4RE        = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)(?::\d{1,5})?\b`)
	ipv6RE        = regexp.MustCompile(`\b(?:[0-9a-fA-F]{1,4}:){2,7}[0-9a-fA-F]{0,4}(?:%[\w.-]+)?(?:\]:?\d{1,5}|:\d{1,5})?\b`)
	urlUserInfoRE = regexp.MustCompile(`(?i)(https?://)[^\s/@:]+:[^\s/@]+@`)
	secretPairRE  = regexp.MustCompile(`(?i)\b(api[_-]?key|token|access[_-]?token|refresh[_-]?token|secret|password|passwd|pwd|authorization)\b\s*[:=]\s*[^\s,;]+`)
	secretQueryRE = regexp.MustCompile(`(?i)([?&](?:api[_-]?key|token|access[_-]?token|refresh[_-]?token|secret|password|passwd|pwd)=)[^\s&#]+`)
	bearerRE      = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*`)
	connectCodeRE = regexp.MustCompile(`(?i)\bOSW(?:-?[0-9A-HJKMNP-TV-Z]){20}\b`)
	emailRE       = regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
	phoneRE       = regexp.MustCompile(`\b(?:\+?\d[\d .()\-]{7,}\d)\b`)
	linuxHomeRE   = regexp.MustCompile(`(/home/)[^/\s]+`)
	macHomeRE     = regexp.MustCompile(`(/Users/)[^/\s]+`)
)

// SafeErrorText returns an error string with common secrets and personal data redacted.
func SafeErrorText(err error) string {
	if err == nil {
		return FallbackErrorText
	}
	return SafeText(err.Error())
}

// SafeText returns text with common secrets and personal data redacted.
func SafeText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return FallbackErrorText
	}

	text = urlUserInfoRE.ReplaceAllString(text, `${1}[redacted]@`)
	text = bearerRE.ReplaceAllString(text, "Bearer [redacted]")
	text = connectCodeRE.ReplaceAllString(text, "OSW-[redacted]")
	text = secretPairRE.ReplaceAllString(text, `${1}=[redacted]`)
	text = secretQueryRE.ReplaceAllString(text, `${1}[redacted]`)
	text = emailRE.ReplaceAllString(text, "[redacted-email]")
	text = linuxHomeRE.ReplaceAllString(text, `${1}[redacted]`)
	text = macHomeRE.ReplaceAllString(text, `${1}[redacted]`)
	text = ipv4RE.ReplaceAllString(text, "[redacted-ip]")
	text = ipv6RE.ReplaceAllString(text, "[redacted-ip]")
	text = phoneRE.ReplaceAllString(text, "[redacted-phone]")

	text = strings.TrimSpace(text)
	if text == "" {
		return FallbackErrorText
	}
	return text
}
