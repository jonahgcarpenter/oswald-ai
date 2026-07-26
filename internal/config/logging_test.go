package config

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func captureLog(t *testing.T, logger *Logger, emit func()) map[string]any {
	t.Helper()
	var output bytes.Buffer
	logger.SetOutput(&output)
	emit()
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatalf("decode log: %v; output=%q", err, output.String())
	}
	return record
}

func assertEnvelope(t *testing.T, record map[string]any) {
	t.Helper()
	for _, key := range []string{"ts", "level", "service", "log_type", "component", "event", "msg"} {
		value, ok := record[key]
		if !ok {
			t.Fatalf("missing envelope field %q: %#v", key, record)
		}
		if _, ok := value.(string); !ok {
			t.Fatalf("envelope field %q has type %T, want string", key, value)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, record["ts"].(string)); err != nil {
		t.Fatalf("invalid timestamp: %v", err)
	}
}

func TestLoggerEnvelopeAndReservedFieldsCannotBeOverwritten(t *testing.T) {
	logger := NewLogger(LevelDebug).Server("contract",
		F("service", "attacker"), F("component", "attacker"), F("event", "attacker"),
	)
	record := captureLog(t, logger, func() {
		logger.Info("contract.complete", "contract message",
			F("ts", "attacker"), F("level", "attacker"), F("service", "attacker"),
			F("log_type", "attacker"), F("component", "attacker"), F("event", "attacker"), F("msg", "attacker"),
			F("status", "ok"),
		)
	})
	assertEnvelope(t, record)
	want := map[string]string{
		"level": "info", "service": serviceName, "log_type": "server", "component": "contract",
		"event": "contract.complete", "msg": "contract message", "status": "ok",
	}
	for key, value := range want {
		if record[key] != value {
			t.Fatalf("%s=%v, want %q", key, record[key], value)
		}
	}
}

func TestLoggerRootAndAgentEnvelopesAreComplete(t *testing.T) {
	for name, logger := range map[string]*Logger{
		"root":  NewLogger(LevelDebug),
		"agent": NewLogger(LevelDebug).Agent("agent", "request", "session", "user", "gateway", "model"),
	} {
		t.Run(name, func(t *testing.T) {
			record := captureLog(t, logger, func() { logger.Debug("contract.event", "message") })
			assertEnvelope(t, record)
		})
	}
}

func TestLoggerAgentFoundationCannotBeOverwritten(t *testing.T) {
	logger := NewLogger(LevelDebug).With(
		F("request_id", "inherited-request"), F("session_id", "inherited-session"),
		F("user_id", "inherited-user"), F("gateway", "inherited-gateway"), F("model", "inherited-model"),
	).Agent("agent-component", "request", "session", "user", "gateway", "model",
		F("request_id", "scoped-request"), F("session_id", "scoped-session"),
		F("user_id", "scoped-user"), F("gateway", "scoped-gateway"), F("model", "scoped-model"),
	).With(
		F("request_id", "later-request"), F("session_id", "later-session"),
		F("user_id", "later-user"), F("gateway", "later-gateway"), F("model", "later-model"),
	)
	record := captureLog(t, logger, func() {
		logger.Info("agent.complete", "message",
			F("request_id", "event-request"), F("session_id", "event-session"),
			F("user_id", "event-user"), F("gateway", "event-gateway"), F("model", "event-model"),
			F("log_type", "server"), F("component", "event-component"),
		)
	})
	want := map[string]string{
		"request_id": "request", "session_id": "session", "user_id": "user",
		"gateway": "gateway", "model": "model", "log_type": "agent", "component": "agent-component",
	}
	for key, value := range want {
		if record[key] != value {
			t.Fatalf("%s=%v, want %q; record=%#v", key, record[key], value, record)
		}
	}
}

func TestLoggerNormalizesStatusVocabulary(t *testing.T) {
	valid := []string{"ok", "error", "rejected", "retry", "degraded"}
	for _, status := range valid {
		logger := NewLogger(LevelDebug)
		record := captureLog(t, logger, func() { logger.Info("status.test", "message", F("status", status)) })
		if record["status"] != status {
			t.Fatalf("status=%v, want %q", record["status"], status)
		}
	}
	logger := NewLogger(LevelDebug)
	record := captureLog(t, logger, func() { logger.Info("status.test", "message", F("status", "completed")) })
	if record["status"] != "degraded" {
		t.Fatalf("invalid status normalized to %v", record["status"])
	}
}

func TestLoggerMarshalFailureHasCompleteEnvelope(t *testing.T) {
	logger := NewLogger(LevelDebug).Agent("contract", "request", "session", "user", "gateway", "model")
	record := captureLog(t, logger, func() {
		logger.Info("original.event", "original message", F("unmarshalable", make(chan int)))
	})
	assertEnvelope(t, record)
	if record["event"] != "logger.marshal_failed" || record["status"] != "error" || record["log_type"] != "agent" || record["component"] != "contract" {
		t.Fatalf("unexpected fallback envelope: %#v", record)
	}
	for key, value := range map[string]string{
		"request_id": "request", "session_id": "session", "user_id": "user", "gateway": "gateway", "model": "model",
	} {
		if record[key] != value {
			t.Fatalf("fallback %s=%v, want %q; record=%#v", key, record[key], value, record)
		}
	}
}

func TestLoggerSecretPromptAndMCPCanariesAreRedacted(t *testing.T) {
	logger := NewLogger(LevelDebug).Server("canary")
	record := captureLog(t, logger, func() {
		logger.Error("canary.failed", "safe fixed message", ErrorField(assertionError("password=mcp-secret token=prompt-secret user=person@example.com endpoint=https://mcp-user:mcp-password@example.com/tools?api_key=mcp-api-key")))
	})
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{"mcp-secret", "prompt-secret", "person@example.com", "mcp-user", "mcp-password", "mcp-api-key"} {
		if strings.Contains(string(raw), canary) {
			t.Fatalf("canary %q leaked: %s", canary, raw)
		}
	}
}

type assertionError string

func (e assertionError) Error() string { return string(e) }
