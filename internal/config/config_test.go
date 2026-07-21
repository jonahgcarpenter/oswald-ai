package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

var retentionEnvKeys = []string{
	"MEMORY_FORGOTTEN_CONTENT_GRACE",
	"MEMORY_CONTENT_BEARING_AUDIT_JOB_RETENTION",
	"MEMORY_CONTENT_FREE_TOMBSTONE_RETENTION",
	"MEMORY_RETIRED_INDEX_RETENTION",
	"MEMORY_SESSION_INACTIVITY",
	"MEMORY_CANDIDATE_CONTENT_RETENTION",
	"MEMORY_SUCCESSFUL_JOB_RETENTION",
	"MEMORY_DEAD_JOB_RETENTION",
	"MEMORY_ACCOUNT_CHALLENGE_GRACE",
	"MEMORY_MAINTENANCE_INTERVAL",
	"MEMORY_DATABASE_OPTIMIZE_INTERVAL",
	"MEMORY_MAINTENANCE_BATCH_SIZE",
}

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

func TestLoadReadsWebSocketAuthenticationConfig(t *testing.T) {
	t.Setenv("WEBSOCKET_AUTH_SIGNING_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("WEBSOCKET_AUTH_MAX_TOKEN_TTL", "10m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WebSocketAuthSigningKey != "0123456789abcdef0123456789abcdef" || cfg.WebSocketAuthMaxTokenTTL != 10*time.Minute {
		t.Fatalf("unexpected websocket auth config: key_set=%t ttl=%s", cfg.WebSocketAuthSigningKey != "", cfg.WebSocketAuthMaxTokenTTL)
	}
}

func TestLoadRejectsInvalidWebSocketTokenTTL(t *testing.T) {
	t.Setenv("WEBSOCKET_AUTH_MAX_TOKEN_TTL", "invalid")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid websocket token TTL error")
	}
}

func TestLoadBackgroundMemoryExtractionConfig(t *testing.T) {
	previous, existed := os.LookupEnv("MEMORY_BACKGROUND_EXTRACTION_ENABLED")
	if err := os.Unsetenv("MEMORY_BACKGROUND_EXTRACTION_ENABLED"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv("MEMORY_BACKGROUND_EXTRACTION_ENABLED", previous)
		} else {
			_ = os.Unsetenv("MEMORY_BACKGROUND_EXTRACTION_ENABLED")
		}
	})
	cfg, err := Load()
	if err != nil || !cfg.BackgroundMemoryExtractionEnabled {
		t.Fatalf("default enabled=%v err=%v", cfg != nil && cfg.BackgroundMemoryExtractionEnabled, err)
	}
	t.Setenv("MEMORY_BACKGROUND_EXTRACTION_ENABLED", "false")
	cfg, err = Load()
	if err != nil || cfg.BackgroundMemoryExtractionEnabled {
		t.Fatalf("configured enabled=%v err=%v", cfg != nil && cfg.BackgroundMemoryExtractionEnabled, err)
	}
	t.Setenv("MEMORY_BACKGROUND_EXTRACTION_ENABLED", "sometimes")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MEMORY_BACKGROUND_EXTRACTION_ENABLED") {
		t.Fatalf("invalid boolean error=%v", err)
	}
}

func TestLoadRetentionPolicyDefaults(t *testing.T) {
	unsetRetentionEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := RetentionPolicy{
		ForgottenContentGrace:           720 * time.Hour,
		ContentBearingAuditJobRetention: 720 * time.Hour,
		ContentFreeTombstoneRetention:   8760 * time.Hour,
		RetiredIndexRetention:           168 * time.Hour,
		SessionInactivity:               24 * time.Hour,
		CandidateContentRetention:       720 * time.Hour,
		SuccessfulJobRetention:          168 * time.Hour,
		DeadJobRetention:                720 * time.Hour,
		AccountChallengeGrace:           24 * time.Hour,
		MaintenanceInterval:             time.Hour,
		DatabaseOptimizeInterval:        24 * time.Hour,
		BatchSize:                       100,
	}
	if cfg.RetentionPolicy != want {
		t.Fatalf("RetentionPolicy = %+v, want %+v", cfg.RetentionPolicy, want)
	}
}

func TestLoadRetentionPolicyOverrides(t *testing.T) {
	unsetRetentionEnv(t)
	overrides := map[string]string{
		"MEMORY_FORGOTTEN_CONTENT_GRACE":             "1h",
		"MEMORY_CONTENT_BEARING_AUDIT_JOB_RETENTION": "2h",
		"MEMORY_CONTENT_FREE_TOMBSTONE_RETENTION":    "3h",
		"MEMORY_RETIRED_INDEX_RETENTION":             "4h",
		"MEMORY_SESSION_INACTIVITY":                  "5h",
		"MEMORY_CANDIDATE_CONTENT_RETENTION":         "6h",
		"MEMORY_SUCCESSFUL_JOB_RETENTION":            "7h",
		"MEMORY_DEAD_JOB_RETENTION":                  "8h",
		"MEMORY_ACCOUNT_CHALLENGE_GRACE":             "9h",
		"MEMORY_MAINTENANCE_INTERVAL":                "10h",
		"MEMORY_DATABASE_OPTIMIZE_INTERVAL":          "11h",
		"MEMORY_MAINTENANCE_BATCH_SIZE":              "12",
	}
	for key, value := range overrides {
		t.Setenv(key, value)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := RetentionPolicy{
		ForgottenContentGrace:           time.Hour,
		ContentBearingAuditJobRetention: 2 * time.Hour,
		ContentFreeTombstoneRetention:   3 * time.Hour,
		RetiredIndexRetention:           4 * time.Hour,
		SessionInactivity:               5 * time.Hour,
		CandidateContentRetention:       6 * time.Hour,
		SuccessfulJobRetention:          7 * time.Hour,
		DeadJobRetention:                8 * time.Hour,
		AccountChallengeGrace:           9 * time.Hour,
		MaintenanceInterval:             10 * time.Hour,
		DatabaseOptimizeInterval:        11 * time.Hour,
		BatchSize:                       12,
	}
	if cfg.RetentionPolicy != want {
		t.Fatalf("RetentionPolicy = %+v, want %+v", cfg.RetentionPolicy, want)
	}
}

func TestLoadRejectsInvalidRetentionValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "empty duration", key: "MEMORY_FORGOTTEN_CONTENT_GRACE", value: ""},
		{name: "malformed duration", key: "MEMORY_SESSION_INACTIVITY", value: "tomorrow"},
		{name: "zero duration", key: "MEMORY_CANDIDATE_CONTENT_RETENTION", value: "0s"},
		{name: "negative duration", key: "MEMORY_MAINTENANCE_INTERVAL", value: "-1h"},
		{name: "empty integer", key: "MEMORY_MAINTENANCE_BATCH_SIZE", value: ""},
		{name: "malformed integer", key: "MEMORY_MAINTENANCE_BATCH_SIZE", value: "many"},
		{name: "zero integer", key: "MEMORY_MAINTENANCE_BATCH_SIZE", value: "0"},
		{name: "negative integer", key: "MEMORY_MAINTENANCE_BATCH_SIZE", value: "-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unsetRetentionEnv(t)
			t.Setenv(tt.key, tt.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("Load error = %v, want error naming %s", err, tt.key)
			}
		})
	}
}

func TestLoadRejectsInvalidRetentionRelationships(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		wantKey   string
	}{
		{
			name: "tombstone shorter than content",
			overrides: map[string]string{
				"MEMORY_CONTENT_BEARING_AUDIT_JOB_RETENTION": "2h",
				"MEMORY_CONTENT_FREE_TOMBSTONE_RETENTION":    "1h",
			},
			wantKey: "MEMORY_CONTENT_FREE_TOMBSTONE_RETENTION",
		},
		{
			name: "dead job shorter than successful job",
			overrides: map[string]string{
				"MEMORY_SUCCESSFUL_JOB_RETENTION": "2h",
				"MEMORY_DEAD_JOB_RETENTION":       "1h",
			},
			wantKey: "MEMORY_DEAD_JOB_RETENTION",
		},
		{
			name: "optimize shorter than maintenance",
			overrides: map[string]string{
				"MEMORY_MAINTENANCE_INTERVAL":       "2h",
				"MEMORY_DATABASE_OPTIMIZE_INTERVAL": "1h",
			},
			wantKey: "MEMORY_DATABASE_OPTIMIZE_INTERVAL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unsetRetentionEnv(t)
			for key, value := range tt.overrides {
				t.Setenv(key, value)
			}
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tt.wantKey) {
				t.Fatalf("Load error = %v, want relationship error naming %s", err, tt.wantKey)
			}
		})
	}
}

func unsetRetentionEnv(t *testing.T) {
	t.Helper()
	for _, key := range retentionEnvKeys {
		value, exists := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
		t.Cleanup(func() {
			if exists {
				if err := os.Setenv(key, value); err != nil {
					t.Errorf("restore %s: %v", key, err)
				}
				return
			}
			if err := os.Unsetenv(key); err != nil {
				t.Errorf("unset restored %s: %v", key, err)
			}
		})
	}
}
