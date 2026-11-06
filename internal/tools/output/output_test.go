package output

import (
	"strings"
	"testing"
	"time"
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

func TestFormatModelTimeCarriesZone(t *testing.T) {
	instant := time.Date(2026, time.November, 5, 22, 15, 0, 0, time.UTC)
	got := FormatModelTime(instant)
	if !strings.Contains(got, "UTC") {
		t.Fatalf("model time missing zone: %q", got)
	}
	if FormatModelTime(time.Time{}) != "" {
		t.Fatal("zero time should render empty")
	}
	loc := time.FixedZone("EST", -5*3600)
	if want := instant.In(loc).Format("Monday, January 02, 2006 at 03:04 PM MST (UTC-07:00)"); FormatModelTime(instant) == want {
		t.Fatal("install-local rendering unexpectedly matched a hardcoded zone")
	}
}

func TestFormatModelTimestampAnnotatesUTC(t *testing.T) {
	got := FormatModelTimestamp("2026-11-06T00:15:00Z")
	if !strings.HasPrefix(got, "2026-11-06T00:15:00Z (") || !strings.Contains(got, "UTC") {
		t.Fatalf("timestamp not annotated: %q", got)
	}
	for _, in := range []string{"", "-", "not-a-time"} {
		if FormatModelTimestamp(in) != in {
			t.Fatalf("unparseable timestamp altered: %q", in)
		}
	}
}
