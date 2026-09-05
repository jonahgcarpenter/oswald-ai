package config

import (
	"errors"
	"io"
	"os"
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

func TestErrorFieldLogRedactsCanariesAndPreservesStructure(t *testing.T) {
	oldStderr := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = writer
	defer func() { os.Stderr = oldStderr }()

	log := NewLogger(LevelDebug).Server("canary")
	log.Error("canary.failed", "canary operation failed",
		F("http_status", 502),
		F("status", "error"),
		ErrorField(errors.New("password=hunter2 user=alice@example.com token=abc123")),
	)
	_ = writer.Close()
	output, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	for _, forbidden := range []string{"hunter2", "alice@example.com", "abc123"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("sensitive canary %q leaked in log: %s", forbidden, text)
		}
	}
	for _, required := range []string{`"component":"canary"`, `"event":"canary.failed"`, `"http_status":502`, `"status":"error"`, `"error":"password=[redacted] user=[redacted-email] token=[redacted]"`} {
		if !strings.Contains(text, required) {
			t.Fatalf("structured field %q missing from log: %s", required, text)
		}
	}
}
