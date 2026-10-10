package agent

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	imagegenerate "github.com/jonahgcarpenter/oswald-ai/internal/tools/image_generate"
	visionanalyze "github.com/jonahgcarpenter/oswald-ai/internal/tools/vision_analyze"
)

func foregroundToolCall(tc llm.ToolCall, decision governance.Decision, result governance.Result, execErr error, toolContent string, executedAt time.Time) memory.ToolHistoryCall {
	args := tc.Function.Arguments
	if name := strings.TrimSpace(tc.Function.Name); name == visionanalyze.Name || name == imagegenerate.Name {
		args = imageCheckpointArguments(args)
	}
	call := memory.ToolHistoryCall{
		Name:           strings.TrimSpace(tc.Function.Name),
		ProviderCallID: strings.TrimSpace(tc.ID),
		HistoryMode:    string(governance.HistoryFull),
		Arguments:      args,
		Status:         "succeeded",
		Outcome:        string(result.Outcome),
		ReasonCode:     result.ReasonCode,
		IsDegraded:     result.IsDegraded,
		Result:         toolContent,
		ExecutedAt:     executedAt.Format(time.RFC3339Nano),
	}
	if !decision.Allowed {
		call.Status = "blocked"
		call.Outcome = ""
		call.ReasonCode = decision.ReasonCode
	} else if execErr != nil {
		call.Status = "failed"
		call.Outcome = ""
		call.ReasonCode = "execution_error"
	}
	return call
}

func persistedToolCall(tc llm.ToolCall, policy governance.HistoryPolicy, decision governance.Decision, result governance.Result, execErr error, toolContent string, executedAt time.Time) memory.ToolHistoryCall {
	call := foregroundToolCall(tc, decision, result, execErr, toolContent, executedAt)
	call.HistoryMode = string(policy.Mode)
	call.SearchResult = policy.SearchResult
	if policy.Mode == governance.HistoryMetadata {
		call.Arguments = map[string]interface{}{}
		call.Result = "Historical tool result omitted by policy."
		call.ArgumentsTruncated = true
		call.ResultTruncated = true
		call.SearchResult = false
		return call
	}
	if call.Arguments == nil {
		call.Arguments = map[string]interface{}{}
	}
	if encoded, err := json.Marshal(call.Arguments); err != nil || len(encoded) > policy.MaxArgumentBytes {
		call.Arguments = map[string]interface{}{}
		call.ArgumentsTruncated = true
	}
	runes := []rune(call.Result)
	if len(runes) > policy.MaxResultRunes {
		notice := []rune("\n[Historical result truncated.]")
		keep := policy.MaxResultRunes - len(notice)
		if keep > 0 {
			call.Result = string(runes[:keep]) + string(notice)
		} else {
			call.Result = string(notice[:policy.MaxResultRunes])
		}
		call.ResultTruncated = true
	}
	return call
}
