package registry

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestRejectedDefinitionDoesNotLogPrivateNameOrDescription(t *testing.T) {
	const canary = "private_schema_prose"
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		var output bytes.Buffer
		log := config.NewLogger(level)
		log.SetOutput(&output)
		reg := New(log)
		if err := reg.RegisterDefinition(definition(canary, " ")); err == nil {
			t.Fatal("invalid definition accepted")
		}
		if reg.Count() != 0 || strings.Contains(output.String(), canary) {
			t.Fatalf("rejected definition or unsafe logs: %s", output.String())
		}
	}
}

func TestRegisteredDefinitionDebugLogContainsOnlySafeFields(t *testing.T) {
	const canary = "private_schema_prose"
	var output bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&output)
	reg := New(log)
	if err := reg.RegisterDefinition(definition("test.safe", canary)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), canary) {
		t.Fatalf("description leaked: %s", output.String())
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record["event"] != "tool.registry.definition_registered" {
		t.Fatalf("unexpected registration log: %+v", record)
	}
	details, _ := record["details"].(map[string]any)
	if details["tool_name"] != "test.safe" {
		t.Fatalf("unexpected registration log: %+v", record)
	}
}
