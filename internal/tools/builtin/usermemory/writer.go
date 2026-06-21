package usermemory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

// ProcessTurnRequest contains the completed exchange used to update memory.
type ProcessTurnRequest struct {
	RequestID     string
	SessionID     string
	UserID        string
	UserText      string
	AssistantText string
	ToolNames     []string
	SessionTTL    time.Duration
}

type extractionOutput struct {
	SessionUpdates struct {
		Summary     string   `json:"summary"`
		OpenThreads []string `json:"open_threads"`
		Decisions   []string `json:"decisions"`
		UserGoals   []string `json:"user_goals"`
	} `json:"session_updates"`
	MemoryCandidates []struct {
		Action     string  `json:"action"`
		Scope      string  `json:"scope"`
		Category   string  `json:"category"`
		Statement  string  `json:"statement"`
		Evidence   string  `json:"evidence"`
		Importance int     `json:"importance"`
		Confidence float64 `json:"confidence"`
		TTLDays    int     `json:"ttl_days"`
		Supersedes string  `json:"supersedes"`
	} `json:"memory_candidates"`
}

// ProcessTurn persists the session turn and asks the model for structured memory updates.
func (s *Store) ProcessTurn(ctx context.Context, client llm.Chatter, model string, req ProcessTurnRequest) error {
	if strings.TrimSpace(req.UserID) == "" || strings.TrimSpace(req.AssistantText) == "" {
		return nil
	}
	if err := s.AppendSessionTurn(ctx, req.SessionID, req.UserID, req.UserText, req.AssistantText, req.ToolNames, req.SessionTTL); err != nil {
		return err
	}
	return s.ProcessTurnMemoryUpdates(ctx, client, model, req)
}

// ProcessTurnMemoryUpdates asks the model for structured memory updates after a
// completed turn. The session turn itself must already be persisted by the caller.
func (s *Store) ProcessTurnMemoryUpdates(ctx context.Context, client llm.Chatter, model string, req ProcessTurnRequest) error {
	if strings.TrimSpace(req.UserID) == "" || strings.TrimSpace(req.AssistantText) == "" {
		return nil
	}
	if client == nil || strings.TrimSpace(model) == "" {
		return nil
	}
	currentSummary, _ := s.ReadSessionSummary(req.SessionID)
	out, err := s.extractMemoryUpdates(ctx, client, model, req, currentSummary)
	if err != nil {
		if s.log != nil {
			s.log.Warn("memory.extraction.failed", "failed to extract memory updates", config.ErrorField(err))
		}
		return nil
	}
	if strings.TrimSpace(out.SessionUpdates.Summary) != "" || len(out.SessionUpdates.OpenThreads) > 0 || len(out.SessionUpdates.Decisions) > 0 || len(out.SessionUpdates.UserGoals) > 0 {
		if err := s.UpsertSessionSummary(SessionSummary{
			SessionID:   req.SessionID,
			UserID:      req.UserID,
			Summary:     out.SessionUpdates.Summary,
			OpenThreads: cleanList(out.SessionUpdates.OpenThreads),
			Decisions:   cleanList(out.SessionUpdates.Decisions),
			UserGoals:   cleanList(out.SessionUpdates.UserGoals),
		}); err != nil {
			return err
		}
	}
	for _, candidate := range out.MemoryCandidates {
		action := strings.TrimSpace(strings.ToLower(candidate.Action))
		if action == "" || action == "ignore" {
			continue
		}
		if action == "forget" {
			_, _ = s.Forget(req.UserID, candidate.Statement, candidate.Scope)
			continue
		}
		if action != "create" && action != "update" && action != "reinforce" {
			continue
		}
		if strings.TrimSpace(candidate.Statement) == "" {
			continue
		}
		ttl := time.Duration(0)
		if candidate.TTLDays > 0 {
			ttl = time.Duration(candidate.TTLDays) * 24 * time.Hour
		}
		_, _ = s.SaveMemory(ctx, req.UserID, SaveRequest{
			Scope:           candidate.Scope,
			Category:        candidate.Category,
			Statement:       candidate.Statement,
			Evidence:        candidate.Evidence,
			Confidence:      candidate.Confidence,
			Importance:      candidate.Importance,
			SourceSessionID: req.SessionID,
			TTL:             ttl,
			Supersedes:      candidate.Supersedes,
		})
	}
	return nil
}

func (s *Store) extractMemoryUpdates(ctx context.Context, client llm.Chatter, model string, req ProcessTurnRequest, current SessionSummary) (extractionOutput, error) {
	prompt := fmt.Sprintf(`You update an assistant memory system after a completed turn.

Return strict JSON only with this shape:
{
  "session_updates": {
    "summary": "concise rolling summary of the active session",
    "open_threads": ["unresolved topics or tasks"],
    "decisions": ["decisions made in this session"],
    "user_goals": ["current user goals"]
  },
  "memory_candidates": [
    {
      "action": "create|update|reinforce|forget|ignore",
      "scope": "short_term|long_term",
      "category": "identity|system_rules|communication_preferences|durable_preferences|projects|relationships|environment|tasks|notes",
      "statement": "grounded third-person memory",
      "evidence": "quote or concise evidence from the turn",
      "importance": 1,
      "confidence": 0.0,
      "ttl_days": 30,
      "supersedes": "older exact statement if contradicted, else empty"
    }
  ]
}

Rules:
- Do not invent facts.
- Use short_term for active projects, temporary plans, unresolved tasks, and context likely to expire.
- Use long_term only for explicit durable instructions, stable identity, stable preferences, or repeated durable facts.
- Use system_rules only for explicit instructions about assistant behavior.
- Use ignore for weak, inferred, trivial, or one-off facts.
- Keep memory statements concise and third-person.
- For short_term, set ttl_days between 7 and 90. For long_term, set ttl_days to 0.

Current session summary:
%s

User message:
%s

Assistant response:
%s

Tools used: %s`, strings.TrimSpace(current.Summary), strings.TrimSpace(req.UserText), strings.TrimSpace(req.AssistantText), strings.Join(req.ToolNames, ", "))

	resp, err := client.Chat(ctx, llm.ChatRequest{
		Model:    model,
		Format:   "json_object",
		Messages: []llm.ChatMessage{{Role: "user", Content: prompt}},
	}, nil)
	if err != nil {
		return extractionOutput{}, err
	}
	var out extractionOutput
	if err := json.Unmarshal([]byte(strings.TrimSpace(resp.Message.Content)), &out); err != nil {
		return extractionOutput{}, err
	}
	return out, nil
}

func cleanList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
