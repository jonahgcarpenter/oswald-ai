package config

import (
	"errors"
	"strings"
	"testing"
)

func TestSafeTextRedactsSensitiveValues(t *testing.T) {
	got := SafeText("token=abc123 email me@example.com http://user:pass@example.com/path?api_key=secret /home/alice/file 192.168.1.2 Bearer deadbeef /connect OSW-0123-4567-89AB-CDEF-GHJK OSW0123456789ABCDEFGHJK")
	for _, forbidden := range []string{"abc123", "me@example.com", "user:pass", "secret", "/home/alice", "192.168.1.2", "deadbeef", "0123-4567", "OSW0123"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("expected %q redacted from %q", forbidden, got)
		}
	}
}

func TestSafeErrorTextFallback(t *testing.T) {
	if SafeErrorText(nil) != FallbackErrorText {
		t.Fatal("expected nil error fallback")
	}
	if SafeErrorText(errors.New("password=hunter2")) != "password=[redacted]" {
		t.Fatalf("unexpected safe error text")
	}
}
