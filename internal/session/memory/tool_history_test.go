package memory

import (
	"strings"
	"testing"
)

func TestToolHistoryRoundTripAndSearchProjection(t *testing.T) {
	history := ToolHistory{Version: ToolHistoryVersion, Batches: []ToolHistoryBatch{{
		AssistantContent: "Checking now.",
		Calls: []ToolHistoryCall{{
			Name: "weather.current", Arguments: map[string]interface{}{"city": "Louisville", "private_argument": "do-not-index"},
			Status: "succeeded", Outcome: "productive", Result: "Forecast marker zephyr", ExecutedAt: "2026-08-28T12:00:00Z", SearchResult: true,
		}},
	}}}
	encoded, searchText, err := EncodeToolHistory(history)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(searchText, "weather.current") || !strings.Contains(searchText, "zephyr") || strings.Contains(searchText, "do-not-index") {
		t.Fatalf("search projection = %q", searchText)
	}
	decoded, err := DecodeToolHistory(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Batches) != 1 || decoded.Batches[0].Calls[0].Arguments["city"] != "Louisville" {
		t.Fatalf("decoded history = %+v", decoded)
	}
}

func TestToolHistoryBoundsAggregateResultsWithoutBreakingCorrelation(t *testing.T) {
	history := EmptyToolHistory()
	for i := 0; i < 10; i++ {
		history.Batches = append(history.Batches, ToolHistoryBatch{Calls: []ToolHistoryCall{{
			Name: "large.tool", Arguments: map[string]interface{}{"value": strings.Repeat("a", 8000)}, Status: "succeeded",
			Result: strings.Repeat("r", 16000), ExecutedAt: "2026-08-28T12:00:00Z", SearchResult: true,
		}}})
	}
	encoded, _, err := EncodeToolHistory(history)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > MaxToolHistoryBytes {
		t.Fatalf("encoded history bytes=%d", len(encoded))
	}
	decoded, err := DecodeToolHistory(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Batches) != 10 {
		t.Fatalf("batch count=%d", len(decoded.Batches))
	}
	for _, batch := range decoded.Batches {
		if len(batch.Calls) != 1 || strings.TrimSpace(batch.Calls[0].Result) == "" {
			t.Fatalf("correlation was dropped: %+v", batch)
		}
	}
}

func TestToolHistoryRejectsIncompleteCalls(t *testing.T) {
	_, _, err := EncodeToolHistory(ToolHistory{Version: ToolHistoryVersion, Batches: []ToolHistoryBatch{{Calls: []ToolHistoryCall{{Status: "succeeded"}}}}})
	if err == nil {
		t.Fatal("incomplete tool call was accepted")
	}
}

func TestBridgeResultHistoryRoundTripAndReplay(t *testing.T) {
	for _, resultName := range []string{"", "mcp__github__create_issue"} {
		history := ToolHistory{Version: 1, Batches: []ToolHistoryBatch{{Calls: []ToolHistoryCall{{Name: "tool_call", ResultName: resultName, ProviderCallID: "call_3", Status: "succeeded", Result: "remote result"}}}}}
		encoded, _, err := EncodeToolHistory(history)
		if err != nil {
			t.Fatal(err)
		}
		if (resultName == "") == strings.Contains(encoded, "result_name") {
			t.Fatal("optional result name persistence changed")
		}
		decoded, err := DecodeToolHistory(encoded)
		if err != nil {
			t.Fatal(err)
		}
		messages := SessionTurnMessages(SessionTurn{ID: 3, ToolHistory: decoded})
		want := "tool_call"
		if resultName != "" {
			want = resultName
		}
		if messages[1].ToolCalls[0].Function.Name != "tool_call" || messages[2].ToolName != want || messages[2].ToolCallID != messages[1].ToolCalls[0].ID || messages[2].Content != "remote result" {
			t.Fatalf("replay changed result or invocation: %+v", messages)
		}
	}
}

func TestToolHistoryRejectsInvalidResultNames(t *testing.T) {
	for _, call := range []ToolHistoryCall{
		{Name: "memory", ResultName: "mcp__home__read", Status: "succeeded"},
		{Name: "tool_call", ResultName: "mcp__home__read", Status: "failed"},
		{Name: "tool_call", ResultName: "mcp__home__read\nsecret", Status: "succeeded"},
		{Name: "tool_call", ResultName: "local", Status: "succeeded"},
	} {
		if _, _, err := EncodeToolHistory(ToolHistory{Version: 1, Batches: []ToolHistoryBatch{{Calls: []ToolHistoryCall{call}}}}); err == nil {
			t.Fatal("invalid result identity persisted")
		}
	}
}
