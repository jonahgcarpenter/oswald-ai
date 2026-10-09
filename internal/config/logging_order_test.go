package config

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestLogFieldOrderAndEscaping(t *testing.T) {
	payload := map[string]any{
		"ts": "now", "level": "warn", "event": "chat.failed", "component": "runtime",
		"user_id": "alice", "gateway": "discord", "request_id": "req",
		"z_count": 2, "a_count": 1, "outcome": "error", "duration_ms": 5,
		"queue_ms": 2, "error_code": "unknown_error", "msg": "quoted \" and newline\n",
	}
	want := []string{"ts", "level", "event", "component", "user_id", "gateway", "request_id", "msg", "a_count", "z_count", "outcome", "duration_ms", "queue_ms", "error_code"}
	for i := 0; i < 20; i++ {
		encoded, err := marshalOrderedLog(payload)
		if err != nil || bytes.Contains(encoded, []byte("\n")) {
			t.Fatalf("invalid single-line record: %s (%v)", encoded, err)
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		_, _ = decoder.Token()
		var keys []string
		for decoder.More() {
			key, _ := decoder.Token()
			keys = append(keys, key.(string))
			var value any
			if err := decoder.Decode(&value); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(keys, want) {
			t.Fatalf("keys=%v want=%v", keys, want)
		}
	}
}

func TestInfoActivityOmitsDevelopmentTelemetry(t *testing.T) {
	var output bytes.Buffer
	log := NewLogger(LevelInfo)
	log.SetOutput(&output)
	log.Server("broker").Debug("broker.started", "development gauge", F("queued_count", 3))
	log.Agent("agent", "req", "alice", "discord", "model").Info("tool.completed", "completed tool execution",
		F("tool_name", "memory"), F("outcome", "productive"), F("duration_ms", 7),
		F("image_count", 4), F("prompt_tokens", 123), F("iteration", 2), F("prompt", "private-canary"))
	if strings.Count(output.String(), "\n") != 1 || strings.Contains(output.String(), "private-canary") {
		t.Fatalf("unexpected activity output: %s", output.String())
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"image_count", "prompt_tokens", "iteration", "model", "prompt"} {
		if _, exists := record[key]; exists {
			t.Fatalf("development/private field %q in activity", key)
		}
	}
	for key, want := range map[string]any{"level": "info", "event": "tool.completed", "user_id": "alice", "gateway": "discord", "request_id": "req", "tool_name": "memory", "outcome": "productive", "duration_ms": float64(7)} {
		if record[key] != want {
			t.Fatalf("%s=%v want=%v", key, record[key], want)
		}
	}
}
