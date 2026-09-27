package agent

import (
	"testing"
	"time"
)

func TestGenericToolStreamPayload(t *testing.T) {
	args := map[string]interface{}{"value": "example"}
	payload := toolStreamPayload("test.echo", args, "result", time.Millisecond, false)
	if payload.Name != "test.echo" || payload.DurationMS != 1 || payload.IsError || payload.ResultText != "result" || payload.Arguments["value"] != "example" {
		t.Fatalf("unexpected generic tool payload: %+v", payload)
	}
}
