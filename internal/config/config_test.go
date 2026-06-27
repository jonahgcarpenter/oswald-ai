package config

import (
	"strings"
	"testing"
	"time"
)

func TestEnvHelpersUseFallbacksForMissingEmptyAndInvalidValues(t *testing.T) {
	t.Setenv("OSWALD_TEST_STRING", "")
	t.Setenv("OSWALD_TEST_INT", "not-an-int")
	t.Setenv("OSWALD_TEST_DURATION", "not-a-duration")

	if got := getEnv("OSWALD_TEST_MISSING", "fallback"); got != "fallback" {
		t.Fatalf("getEnv missing = %q, want fallback", got)
	}
	if got := getEnv("OSWALD_TEST_STRING", "fallback"); got != "" {
		t.Fatalf("getEnv set empty = %q, want empty", got)
	}
	if got := getEnvInt("OSWALD_TEST_INT", 12); got != 12 {
		t.Fatalf("getEnvInt invalid = %d, want 12", got)
	}
	if got := getEnvDuration("OSWALD_TEST_DURATION", 3*time.Second); got != 3*time.Second {
		t.Fatalf("getEnvDuration invalid = %s, want 3s", got)
	}
}

func TestEnvHelpersParseConfiguredValues(t *testing.T) {
	t.Setenv("OSWALD_TEST_STRING", "value")
	t.Setenv("OSWALD_TEST_INT", "42")
	t.Setenv("OSWALD_TEST_DURATION", "1500ms")

	if got := getEnv("OSWALD_TEST_STRING", "fallback"); got != "value" {
		t.Fatalf("getEnv set = %q, want value", got)
	}
	if got := getEnvInt("OSWALD_TEST_INT", 0); got != 42 {
		t.Fatalf("getEnvInt set = %d, want 42", got)
	}
	if got := getEnvDuration("OSWALD_TEST_DURATION", 0); got != 1500*time.Millisecond {
		t.Fatalf("getEnvDuration set = %s, want 1500ms", got)
	}
}

func TestParseLevelAndRequestID(t *testing.T) {
	if got := ParseLevel(" warning "); got != LevelWarn {
		t.Fatalf("ParseLevel warning = %s, want warn", got)
	}
	if got := ParseLevel("unknown"); got != LevelInfo {
		t.Fatalf("ParseLevel unknown = %s, want info", got)
	}

	id := NewRequestID()
	if !strings.HasPrefix(id, "req_") || len(id) != len("req_")+16 {
		t.Fatalf("NewRequestID() = %q, want req_ plus 16 hex chars", id)
	}
}
