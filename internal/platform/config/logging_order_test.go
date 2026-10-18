package config

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func orderedJSONKeys(t *testing.T, encoded []byte) (top []string, details []string) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		t.Fatalf("expected top-level object: %s (%v)", encoded, err)
	}
	for decoder.More() {
		keyToken, _ := decoder.Token()
		key := keyToken.(string)
		top = append(top, key)
		if key == "details" {
			nested, err := decoder.Token()
			if err != nil || nested != json.Delim('{') {
				t.Fatalf("expected details object: %s (%v)", encoded, err)
			}
			for decoder.More() {
				nestedKey, _ := decoder.Token()
				details = append(details, nestedKey.(string))
				var value any
				if err := decoder.Decode(&value); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := decoder.Token(); err != nil {
				t.Fatal(err)
			}
			continue
		}
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
	}
	return top, details
}

func TestLogFieldOrderAndEscaping(t *testing.T) {
	payload := map[string]any{
		"ts": "now", "level": "warn", "event": "chat.failed", "component": "runtime", "log_type": "server",
		"details": map[string]any{
			"msg":     "quoted \" and newline\n",
			"user_id": "alice", "gateway": "discord", "request_id": "req",
			"z_count": 2, "a_count": 1, "outcome": "error", "duration_ms": 5,
			"queue_ms": 2, "error_code": "unknown_error",
		},
	}
	wantTop := []string{"ts", "level", "event", "component", "log_type", "details"}
	wantDetails := []string{"msg", "user_id", "gateway", "request_id", "a_count", "z_count", "outcome", "duration_ms", "queue_ms", "error_code"}
	for i := 0; i < 20; i++ {
		encoded, err := marshalOrderedLog(payload)
		if err != nil || bytes.Contains(encoded, []byte("\n")) {
			t.Fatalf("invalid single-line record: %s (%v)", encoded, err)
		}
		top, details := orderedJSONKeys(t, encoded)
		if !reflect.DeepEqual(top, wantTop) {
			t.Fatalf("top keys=%v want=%v", top, wantTop)
		}
		if !reflect.DeepEqual(details, wantDetails) {
			t.Fatalf("details keys=%v want=%v", details, wantDetails)
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
	details := detailsOf(t, record)
	for _, key := range []string{"image_count", "prompt_tokens", "iteration", "model", "prompt"} {
		if _, exists := details[key]; exists {
			t.Fatalf("development/private field %q in activity", key)
		}
	}
	for key, want := range map[string]any{"level": "info", "event": "tool.completed"} {
		if record[key] != want {
			t.Fatalf("%s=%v want=%v", key, record[key], want)
		}
	}
	for key, want := range map[string]any{"profile": "alice", "gateway": "discord", "request_id": "req", "tool_name": "memory", "outcome": "productive", "duration_ms": float64(7)} {
		if details[key] != want {
			t.Fatalf("%s=%v want=%v", key, details[key], want)
		}
	}
}
