package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	compDefaultPort                = "8080"
	compDefaultPromptDebugPath     = "./tmp/prompt-debug"
	compDefaultUserPrefix          = "memory-test-compaction"
	compTestPrefix                 = "MEMTEST"
	compContextBlobRepeatDefault   = 20000
	compDumpPollInterval           = 200 * time.Millisecond
	compDumpPollAttempts           = 50
	compCompactedHistoryUserPrompt = "Here is the compacted history from previous messages."
)

type compConfig struct {
	port              string
	promptDebugPath   string
	userPrefix        string
	expectedMaxTurns  int
	expectedMaxAge    time.Duration
	contextBlobRepeat int
}

type compAgentResponse struct {
	Model    string `json:"model"`
	Response string `json:"response"`
	Error    string `json:"error"`
}

type compPromptDump struct {
	Path            string
	EstimatedBefore int
	EstimatedAfter  int
	Messages        []compDumpMessage
}

type compDumpMessage struct {
	Role    string
	Content string
}

func main() {
	cfg := compLoadConfig()
	if cfg.expectedMaxTurns <= 0 {
		log.Fatalf("This compaction test requires MEMORY_MAX_TURNS > 0. Current expected value: %d", cfg.expectedMaxTurns)
	}
	if cfg.expectedMaxAge != 0 {
		log.Fatalf("This compaction test expects MEMORY_MAX_AGE=0 so TTL does not interfere. Current expected value: %s", cfg.expectedMaxAge)
	}

	fmt.Println("Memory compaction integration test")
	fmt.Println("--------------------------------------------------------------------------------")
	fmt.Printf("WebSocket endpoint              : ws://localhost:%s/ws\n", cfg.port)
	fmt.Printf("Prompt debug directory          : %s\n", cfg.promptDebugPath)
	fmt.Printf("Expected server MEMORY_MAX_TURNS: %d\n", cfg.expectedMaxTurns)
	fmt.Printf("Expected server MEMORY_MAX_AGE  : %s\n", cfg.expectedMaxAge)
	fmt.Printf("Expected server PROMPT_DEBUG_PATH: %s\n", cfg.promptDebugPath)
	fmt.Printf("Initial large-prompt repeat    : %d\n", cfg.contextBlobRepeat)
	fmt.Println("This test expects MEMORY_MAX_AGE=0 so only compaction and max-turn pruning affect history.")
	fmt.Println("--------------------------------------------------------------------------------")

	u := url.URL{Scheme: "ws", Host: "localhost:" + cfg.port, Path: "/ws"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Fatalf("Failed to connect to %s: %v", u.String(), err)
	}
	defer conn.Close()

	compactionUserID := cfg.userPrefix + "-active"
	dump := compTriggerCompaction(conn, cfg, compactionUserID, compTestPrefix+"-CMP")
	history := compHistoryMessages(dump)
	compAssertHasCompactedPair(history, "budget pressure should replace older history with a compacted turn pair")
	if dump.EstimatedBefore <= dump.EstimatedAfter {
		log.Fatalf("Expected compaction to reduce estimated prompt tokens, got before=%d after=%d", dump.EstimatedBefore, dump.EstimatedAfter)
	}
	fmt.Printf("Compaction validation passed. Estimated before=%d after=%d\n", dump.EstimatedBefore, dump.EstimatedAfter)
	fmt.Printf("Dump: %s\n", dump.Path)

	maxTurnsUserID := cfg.userPrefix + "-max-turns"
	compTriggerCompaction(conn, cfg, maxTurnsUserID, compTestPrefix+"-CMP-MAX")
	for i := 1; i <= cfg.expectedMaxTurns+1; i++ {
		label := fmt.Sprintf("%s-CMP-MAX-FILL-%02d", compTestPrefix, i)
		compSendAndCapture(conn, cfg, maxTurnsUserID, label, compBuildExactAckPrompt(label))
	}

	inspectLabel := compTestPrefix + "-CMP-MAX-INSPECT"
	inspectDump := compSendAndCapture(conn, cfg, maxTurnsUserID, inspectLabel, compBuildExactAckPrompt(inspectLabel))
	compAssertNoCompactedPair(compHistoryMessages(inspectDump), "compacted history should count toward MAX_TURNS and be pruned once it becomes the oldest retained turn")
	fmt.Printf("Compacted MAX_TURNS validation passed. Dump: %s\n", inspectDump.Path)
}

func compLoadConfig() compConfig {
	return compConfig{
		port:              compGetEnv("MEMORY_TEST_PORT", compDefaultPort),
		promptDebugPath:   compGetEnv("MEMORY_TEST_PROMPT_DEBUG_PATH", compDefaultPromptDebugPath),
		userPrefix:        compGetEnv("MEMORY_TEST_USER_PREFIX", compDefaultUserPrefix),
		expectedMaxTurns:  compGetEnvInt("MEMORY_TEST_EXPECTED_MAX_TURNS", 3),
		expectedMaxAge:    compGetEnvDuration("MEMORY_TEST_EXPECTED_MAX_AGE", 0),
		contextBlobRepeat: compGetEnvInt("MEMORY_TEST_CONTEXT_BLOB_REPEAT", compContextBlobRepeatDefault),
	}
}

func compTriggerCompaction(conn *websocket.Conn, cfg compConfig, userID string, prefix string) compPromptDump {
	var lastDump compPromptDump
	repeat := cfg.contextBlobRepeat
	for i := 1; i <= 8; i++ {
		label := fmt.Sprintf("%s-%02d", prefix, i)
		fmt.Printf("sending compaction probe       : label=%s repeat=%d\n", label, repeat)
		lastDump = compSendAndCapture(conn, cfg, userID, label, compBuildLargeContextPrompt(label, repeat))
		fmt.Printf("probe token estimates          : before=%d after=%d\n", lastDump.EstimatedBefore, lastDump.EstimatedAfter)
		if compHasCompactedPair(compHistoryMessages(lastDump)) {
			return lastDump
		}
		repeat *= 2
	}

	log.Fatalf("Failed to trigger compaction for session %q after multiple increasingly large prompts. Increase MEMORY_TEST_CONTEXT_BLOB_REPEAT or use a model with a smaller prompt budget.", userID)
	return compPromptDump{}
}

func compBuildExactAckPrompt(label string) string {
	return fmt.Sprintf("%s\nStore this exact label for memory diagnostics: %s\nReply with exactly: ACK %s", label, label, label)
}

func compBuildLargeContextPrompt(label string, repeat int) string {
	chunk := fmt.Sprintf("%s payload segment for compaction validation. ", label)
	largeBody := strings.Repeat(chunk, repeat)
	return fmt.Sprintf("%s\nRemember this exact label for compaction diagnostics: %s\n%s\nReply with exactly: ACK %s", label, label, largeBody, label)
}

func compSendAndCapture(conn *websocket.Conn, cfg compConfig, userID string, label string, prompt string) compPromptDump {
	knownFiles := compListMatchingPromptDumps(cfg.promptDebugPath, userID)
	resp := compSendPrompt(conn, userID, prompt)
	fmt.Printf("sent %-24s response=%q\n", label, resp.Response)
	return compMustReadLatestPromptDump(cfg.promptDebugPath, userID, knownFiles)
}

func compSendPrompt(conn *websocket.Conn, userID string, prompt string) compAgentResponse {
	msg, err := json.Marshal(struct {
		UserID string `json:"user_id"`
		Prompt string `json:"prompt"`
	}{UserID: userID, Prompt: prompt})
	if err != nil {
		log.Fatalf("Failed to marshal prompt: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		log.Fatalf("Failed to send prompt: %v", err)
	}

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			log.Fatalf("Failed to read response: %v", err)
		}

		var resp compAgentResponse
		if err := json.Unmarshal(raw, &resp); err == nil && resp.Model != "" {
			if resp.Error != "" {
				log.Fatalf("Agent returned an error: %s", resp.Error)
			}
			return resp
		}
	}
}

func compMustReadLatestPromptDump(dir string, sessionKey string, knownFiles map[string]struct{}) compPromptDump {
	pattern := filepath.Join(dir, "prompt_"+compSanitizeFilePart(sessionKey, 16)+"_*.md")
	var lastErr error

	for attempt := 0; attempt < compDumpPollAttempts; attempt++ {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			log.Fatalf("Invalid prompt debug glob %q: %v", pattern, err)
		}

		allFiles, err := filepath.Glob(filepath.Join(dir, "*.md"))
		if err != nil {
			log.Fatalf("Invalid prompt debug directory glob for %q: %v", dir, err)
		}

		latestPath := ""
		latestMod := time.Time{}
		for _, path := range matches {
			info, err := os.Stat(path)
			if err != nil {
				lastErr = err
				continue
			}
			if _, seen := knownFiles[path]; seen {
				continue
			}
			if latestPath == "" || info.ModTime().After(latestMod) {
				latestPath = path
				latestMod = info.ModTime()
			}
		}

		if latestPath == "" {
			if len(allFiles) == 0 {
				lastErr = fmt.Errorf("no markdown prompt dumps found in %s; ensure the server is running with PROMPT_DEBUG_PATH=%s and restart it", dir, dir)
			} else {
				lastErr = fmt.Errorf("no new prompt dump found for session %q (pattern %s)", sessionKey, pattern)
			}
			time.Sleep(compDumpPollInterval)
			continue
		}

		raw, err := os.ReadFile(latestPath)
		if err != nil {
			lastErr = err
			time.Sleep(compDumpPollInterval)
			continue
		}

		dump, err := compParsePromptDump(latestPath, string(raw))
		if err != nil {
			lastErr = err
			time.Sleep(compDumpPollInterval)
			continue
		}

		fmt.Printf("reading prompt dump            : %s\n", dump.Path)
		return dump
	}

	log.Fatalf("Failed to read prompt dump for session %q from %s: %v", sessionKey, dir, lastErr)
	return compPromptDump{}
}

func compListMatchingPromptDumps(dir string, sessionKey string) map[string]struct{} {
	pattern := filepath.Join(dir, "prompt_"+compSanitizeFilePart(sessionKey, 16)+"_*.md")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		log.Fatalf("Invalid prompt debug glob %q: %v", pattern, err)
	}
	seen := make(map[string]struct{}, len(matches))
	for _, path := range matches {
		seen[path] = struct{}{}
	}
	return seen
}

func compParsePromptDump(path string, raw string) (compPromptDump, error) {
	dump := compPromptDump{
		Path:            path,
		EstimatedBefore: compParseTableInt(raw, "Estimated tokens (before pruning)"),
		EstimatedAfter:  compParseTableInt(raw, "Estimated tokens (after pruning)"),
	}

	sectionStart := strings.Index(raw, "## Actual Request Sent to Ollama")
	if sectionStart < 0 {
		return dump, fmt.Errorf("prompt dump %s missing request section", path)
	}

	lines := strings.Split(raw[sectionStart:], "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if !strings.HasPrefix(line, "### [") {
			continue
		}

		role, ok := compFirstBacktickValue(line)
		if !ok {
			return dump, fmt.Errorf("prompt dump %s has malformed message header %q", path, line)
		}

		msg := compDumpMessage{Role: role}
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "### [") {
				i = j - 1
				break
			}
			if lines[j] == "```" && msg.Content == "" {
				var content []string
				for k := j + 1; k < len(lines); k++ {
					if lines[k] == "```" {
						msg.Content = strings.Join(content, "\n")
						j = k
						break
					}
					content = append(content, lines[k])
				}
			}
			if j == len(lines)-1 {
				i = j
			}
		}

		dump.Messages = append(dump.Messages, msg)
	}

	if len(dump.Messages) == 0 {
		return dump, fmt.Errorf("prompt dump %s contained no parsed messages", path)
	}
	return dump, nil
}

func compParseTableInt(raw string, label string) int {
	needle := "| " + label + " |"
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, needle) {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 3 {
			break
		}
		value := strings.TrimSpace(parts[2])
		n, err := strconv.Atoi(value)
		if err == nil {
			return n
		}
	}
	return 0
}

func compFirstBacktickValue(s string) (string, bool) {
	start := strings.IndexByte(s, '`')
	if start < 0 {
		return "", false
	}
	end := strings.IndexByte(s[start+1:], '`')
	if end < 0 {
		return "", false
	}
	return s[start+1 : start+1+end], true
}

func compHistoryMessages(dump compPromptDump) []compDumpMessage {
	if len(dump.Messages) <= 2 {
		return nil
	}
	return dump.Messages[1 : len(dump.Messages)-1]
}

func compHasCompactedPair(messages []compDumpMessage) bool {
	for i := 0; i+1 < len(messages); i++ {
		if messages[i].Role == "user" && messages[i].Content == compCompactedHistoryUserPrompt && messages[i+1].Role == "assistant" && strings.TrimSpace(messages[i+1].Content) != "" {
			return true
		}
	}
	return false
}

func compAssertHasCompactedPair(messages []compDumpMessage, reason string) {
	if !compHasCompactedPair(messages) {
		log.Fatalf("Expected compacted pair in history: %s", reason)
	}
}

func compAssertNoCompactedPair(messages []compDumpMessage, reason string) {
	if compHasCompactedPair(messages) {
		log.Fatalf("Expected no compacted pair in history: %s", reason)
	}
}

func compSanitizeFilePart(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) > maxLen {
		runes = runes[:maxLen]
	}
	for i, r := range runes {
		if !compIsFileSafe(r) {
			runes[i] = '_'
		}
	}
	return string(runes)
}

func compIsFileSafe(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '-' || r == '_'
}

func compGetEnv(key string, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}

func compGetEnvInt(key string, fallback int) int {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		n, err := strconv.Atoi(value)
		if err == nil {
			return n
		}
	}
	return fallback
}

func compGetEnvDuration(key string, fallback time.Duration) time.Duration {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		d, err := time.ParseDuration(value)
		if err == nil {
			return d
		}
	}
	return fallback
}
