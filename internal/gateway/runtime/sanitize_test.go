package runtime

import (
	"errors"
	"strings"
	"testing"
)

func TestSafeErrorTextRedactsSensitiveDetails(t *testing.T) {
	text := safeErrorText(errors.New("request to http://user:pass@10.0.0.1:8080 failed: token=abc123 Authorization: Bearer secret-token"))

	for _, disallowed := range []string{"10.0.0.1", "abc123", "secret-token", "user:pass"} {
		if strings.Contains(text, disallowed) {
			t.Fatalf("expected %q to be redacted from %q", disallowed, text)
		}
	}
	for _, expected := range []string{"[redacted-ip]", "token=[redacted]", "Authorization=[redacted]", "http://[redacted]@"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("expected %q in sanitized text %q", expected, text)
		}
	}
}

func TestSafeErrorTextKeepsUsefulMessage(t *testing.T) {
	text := safeErrorText(errors.New("model gateway returned status 502"))
	if text != "model gateway returned status 502" {
		t.Fatalf("unexpected sanitized text: %q", text)
	}
}
