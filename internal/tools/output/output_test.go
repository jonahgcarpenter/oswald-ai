package output

import (
	"strings"
	"testing"
)

func TestStripANSI(t *testing.T) {
	in := "\x1b[32mgreen\x1b[0m plain"
	if got := StripANSI(in); got != "green plain" {
		t.Fatalf("ansi = %q", got)
	}
}

func TestRedactSecrets(t *testing.T) {
	cases := []string{
		"Authorization: Bearer abcdefghijklmnop1234",
		"api_key=abcdefghijklmnop1234",
		"sk-abcdefghijklmnop123456",
	}
	for _, in := range cases {
		got := RedactSecrets(in)
		if strings.Contains(got, "abcdefghijklmnop") || !strings.Contains(got, "[REDACTED]") {
			t.Fatalf("redact(%q) = %q", in, got)
		}
	}
	if got := RedactSecrets("plain output 42"); got != "plain output 42" {
		t.Fatalf("clean text altered: %q", got)
	}
}
