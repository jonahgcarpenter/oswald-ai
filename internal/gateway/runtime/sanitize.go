package runtime

import (
	"regexp"
	"strings"
)

const fallbackErrorText = "An error occurred, but I can't tell you that shit."

var (
	ipv4RE        = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)(?::\d{1,5})?\b`)
	ipv6RE        = regexp.MustCompile(`\b(?:[0-9a-fA-F]{1,4}:){2,7}[0-9a-fA-F]{0,4}(?:%[\w.-]+)?(?:\]:?\d{1,5}|:\d{1,5})?\b`)
	urlUserInfoRE = regexp.MustCompile(`(?i)(https?://)[^\s/@:]+:[^\s/@]+@`)
	secretPairRE  = regexp.MustCompile(`(?i)\b(api[_-]?key|token|access[_-]?token|refresh[_-]?token|secret|password|passwd|pwd|authorization)\b\s*[:=]\s*[^\s,;]+`)
	secretQueryRE = regexp.MustCompile(`(?i)([?&](?:api[_-]?key|token|access[_-]?token|refresh[_-]?token|secret|password|passwd|pwd)=)[^\s&#]+`)
	bearerRE      = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*`)
)

func safeErrorText(err error) string {
	if err == nil {
		return fallbackErrorText
	}

	text := strings.TrimSpace(err.Error())
	if text == "" {
		return fallbackErrorText
	}

	text = urlUserInfoRE.ReplaceAllString(text, `${1}[redacted]@`)
	text = bearerRE.ReplaceAllString(text, "Bearer [redacted]")
	text = secretPairRE.ReplaceAllString(text, `${1}=[redacted]`)
	text = secretQueryRE.ReplaceAllString(text, `${1}[redacted]`)
	text = ipv4RE.ReplaceAllString(text, "[redacted-ip]")
	text = ipv6RE.ReplaceAllString(text, "[redacted-ip]")
	text = strings.TrimSpace(text)
	if text == "" {
		return fallbackErrorText
	}
	return text
}
