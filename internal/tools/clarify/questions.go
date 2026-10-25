package clarify

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Bounds for model-supplied question content. The rendered ask stays
// within a few thousand runes so gateway delivery never balloons.
const (
	maxQuestions       = 5
	maxChoices         = 4
	maxQuestionRunes   = 200
	maxChoiceRunes     = 80
	maxRenderedRunes   = 4000
	statusAnswered     = "answered"
	statusSkipped      = "skipped"
	statusUnanswered   = "unanswered"
	outcomeSubmitted   = "submitted"
	outcomeCancelled   = "cancelled"
	outcomeTimedOut    = "timed_out"
	outcomeUndelivered = "undelivered"
)

// Question is one validated clarify form entry.
type Question struct {
	Question    string
	Choices     []string
	MultiSelect bool
}

// Response is one answer aligned with its question.
type Response struct {
	Question     string      `json:"question"`
	Status       string      `json:"status"`
	UserResponse interface{} `json:"user_response"`
}

// parseQuestions validates the raw questions argument into structured form.
func parseQuestions(raw interface{}) ([]Question, error) {
	items, ok := raw.([]interface{})
	if !ok || len(items) == 0 || len(items) > maxQuestions {
		return nil, fmt.Errorf("questions must contain 1 to %d entries", maxQuestions)
	}
	questions := make([]Question, 0, len(items))
	for i, item := range items {
		entry, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("question %d must be an object", i+1)
		}
		text, _ := entry["question"].(string)
		text = strings.TrimSpace(text)
		if text == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > maxQuestionRunes {
			return nil, fmt.Errorf("question %d must contain 1 to %d characters", i+1, maxQuestionRunes)
		}
		question := Question{Question: text}
		if multi, ok := entry["multi_select"].(bool); ok {
			question.MultiSelect = multi
		}
		if rawChoices, exists := entry["choices"]; exists && rawChoices != nil {
			choices, ok := rawChoices.([]interface{})
			if !ok || len(choices) == 0 || len(choices) > maxChoices {
				return nil, fmt.Errorf("question %d choices must contain 1 to %d entries", i+1, maxChoices)
			}
			for _, rawChoice := range choices {
				choice, _ := rawChoice.(string)
				choice = strings.TrimSpace(choice)
				if choice == "" || !utf8.ValidString(choice) || utf8.RuneCountInString(choice) > maxChoiceRunes {
					return nil, fmt.Errorf("question %d choices must contain 1 to %d characters", i+1, maxChoiceRunes)
				}
				question.Choices = append(question.Choices, choice)
			}
		}
		questions = append(questions, question)
	}
	return questions, nil
}

// ParseAnswer maps one free-text reply onto its question. The whole reply is
// the answer; a leading "N:" number prefix is tolerated and stripped.
// Skipped entries use an explicit skip token.
func ParseAnswer(question Question, text string) Response {
	value := strings.TrimSpace(text)
	if _, remainder, found := splitNumberedLine(value); found {
		value = remainder
	}
	return answerOne(question, value, true)
}

// splitNumberedLine cuts "1: pick" and "2. pick" prefixes into their number
// and remainder.
func splitNumberedLine(line string) (int, string, bool) {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(line) {
		return 0, "", false
	}
	sep := line[i]
	if sep != ':' && sep != '.' && sep != ')' && sep != '-' {
		return 0, "", false
	}
	number, err := strconv.Atoi(line[:i])
	if err != nil {
		return 0, "", false
	}
	return number, strings.TrimSpace(line[i+1:]), true
}

// skipTokens mark an explicit skip.
var skipTokens = map[string]bool{"skip": true, "skipped": true, "n/a": true, "na": true, "none": true}

// answerOne builds one response from its raw value. Single letters resolve
// to their choice text; anything else rides through as free text.
func answerOne(question Question, value string, present bool) Response {
	response := Response{Question: question.Question}
	if !present || value == "" {
		response.Status = statusUnanswered
		return response
	}
	if skipTokens[strings.ToLower(value)] {
		response.Status = statusSkipped
		return response
	}
	if question.MultiSelect {
		var picks []string
		for _, part := range strings.Split(value, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				picks = append(picks, resolveChoice(question, trimmed))
			}
		}
		if len(picks) == 0 {
			response.Status = statusUnanswered
			return response
		}
		response.Status = statusAnswered
		response.UserResponse = picks
		return response
	}
	response.Status = statusAnswered
	response.UserResponse = resolveChoice(question, value)
	return response
}

// resolveChoice maps a single letter (A, B, ...) to its choice text. All
// other input rides through unchanged as free text.
func resolveChoice(question Question, value string) string {
	if len(question.Choices) == 0 {
		return value
	}
	trimmed := strings.TrimSpace(value)
	if len(trimmed) == 1 {
		upper := strings.ToUpper(trimmed)
		if index := int(upper[0] - 'A'); index >= 0 && index < len(question.Choices) {
			return question.Choices[index]
		}
	}
	return value
}

// ResultContent renders the tool result payload.
func ResultContent(responses []Response, outcome, notice string) string {
	payload := map[string]interface{}{"responses": responses, "outcome": outcome}
	if strings.TrimSpace(notice) != "" {
		payload["notice"] = strings.TrimSpace(notice)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return `{"responses":[],"outcome":"` + outcome + `"}`
	}
	return string(encoded)
}

// replyPrompt renders the reply footer: letter-based when the question has
// choices, free-text otherwise.
func replyPrompt(question Question) string {
	if len(question.Choices) > 0 {
		if question.MultiSelect {
			return "Reply with the letter(s) in a comma-separated list, your own answer, or skip."
		}
		return "Reply with the letter, your own answer, or skip."
	}
	return "Reply with your answer or skip."
}

// RenderExpired renders the expiry follow-up: sent only when the wait lapses
// with no answer, so the user learns the ask timed out.
func RenderExpired() string {
	return "Your input request expired with no answer; I continued without it. Ask me to ask again if it still matters."
}

// RenderAskOne renders one parked question as plain text: lettered choices
// with the first marked (Recommended), and an Other row on questions with
// choices. Question text rides through untouched, including any numbering
// the model supplied. The footer alone describes the reply format.
func RenderAskOne(question Question, minutesLeft int) string {
	var sb strings.Builder
	sb.WriteString("I need your input before I continue:\n\n" + question.Question)
	for j, choice := range question.Choices {
		suffix := ""
		if j == 0 {
			suffix = " (Recommended)"
		}
		fmt.Fprintf(&sb, "\n   %c. %s%s", 'A'+j, choice, suffix)
	}
	if len(question.Choices) > 0 {
		fmt.Fprintf(&sb, "\n   %c. Other (reply with your own answer)", 'A'+len(question.Choices))
	}
	if minutesLeft > 1 {
		fmt.Fprintf(&sb, "\n\n%s You have %d minutes.", replyPrompt(question), minutesLeft)
	} else {
		fmt.Fprintf(&sb, "\n\n%s You have about a minute left.", replyPrompt(question))
	}
	rendered := sb.String()
	if runes := []rune(rendered); len(runes) > maxRenderedRunes {
		rendered = string(runes[:maxRenderedRunes]) + "\u2026"
	}
	return rendered
}
