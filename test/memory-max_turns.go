package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	maxTurnsDefaultPort            = "8080"
	maxTurnsDefaultPromptDebugPath = "./tmp/prompt-debug"
	maxTurnsDefaultUserID          = "memory-test-max"
	maxTurnsTestPrefix             = "MEMTEST"
	maxTurnsDumpPollInterval       = 200 * time.Millisecond
	maxTurnsDumpPollAttempts       = 50
)

type maxTurnsConfig struct {
	port             string
	promptDebugPath  string
	userID           string
	expectedMaxTurns int
	expectedMaxAge   time.Duration
}

type maxTurnsAgentResponse struct {
	Model    string `json:"model"`
	Response string `json:"response"`
	Error    string `json:"error"`
}

type maxTurnsPromptDump struct {
	Path     string
	Messages []maxTurnsDumpMessage
}

type maxTurnsDumpMessage struct {
	Role    string
	Content string
}

func main() {
	cfg := maxTurnsLoadConfig()
	if cfg.expectedMaxTurns <= 0 {
		log.Fatalf("This MAX_TURNS test requires MEMORY_MAX_TURNS > 0. Current expected value: %d", cfg.expectedMaxTurns)
	}
	if cfg.expectedMaxAge != 0 {
		log.Fatalf("This MAX_TURNS test expects MEMORY_MAX_AGE=0 so TTL does not interfere. Current expected value: %s", cfg.expectedMaxAge)
	}

	fmt.Println("Memory max-turns integration test")
	fmt.Println("--------------------------------------------------------------------------------")
	fmt.Printf("WebSocket endpoint              : ws://localhost:%s/ws\n", cfg.port)
	fmt.Printf("Prompt debug directory          : %s\n", cfg.promptDebugPath)
	fmt.Printf("Expected server MEMORY_MAX_TURNS: %d\n", cfg.expectedMaxTurns)
	fmt.Printf("Expected server MEMORY_MAX_AGE  : %s\n", cfg.expectedMaxAge)
	fmt.Printf("Expected server PROMPT_DEBUG_PATH: %s\n", cfg.promptDebugPath)
	fmt.Println("This test expects MEMORY_MAX_AGE=0 so only max-turn retention is active.")
	fmt.Println("--------------------------------------------------------------------------------")

	u := url.URL{Scheme: "ws", Host: "localhost:" + cfg.port, Path: "/ws"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Fatalf("Failed to connect to %s: %v", u.String(), err)
	}
	defer conn.Close()

	labels := make([]string, 0, cfg.expectedMaxTurns+1)
	for i := 1; i <= cfg.expectedMaxTurns+1; i++ {
		label := fmt.Sprintf("%s-MAX-%02d", maxTurnsTestPrefix, i)
		labels = append(labels, label)
		maxTurnsSendAndCapture(conn, cfg, label, maxTurnsBuildExactAckPrompt(label))
	}

	inspectLabel := maxTurnsTestPrefix + "-MAX-INSPECT"
	dump := maxTurnsSendAndCapture(conn, cfg, inspectLabel, maxTurnsBuildExactAckPrompt(inspectLabel))
	historyLabels := maxTurnsLabelsInMessages(maxTurnsHistoryMessages(dump))
	expected := labels[1:]

	maxTurnsAssertExactLabels(historyLabels, expected, "MAX_TURNS should keep only the newest retained raw turn pairs")
	fmt.Printf("MAX_TURNS validation passed. Expected retained labels: %s\n", strings.Join(expected, ", "))
	fmt.Printf("Dump: %s\n", dump.Path)
}

func maxTurnsLoadConfig() maxTurnsConfig {
	return maxTurnsConfig{
		port:             maxTurnsGetEnv("MEMORY_TEST_PORT", maxTurnsDefaultPort),
		promptDebugPath:  maxTurnsGetEnv("MEMORY_TEST_PROMPT_DEBUG_PATH", maxTurnsDefaultPromptDebugPath),
		userID:           maxTurnsGetEnv("MEMORY_TEST_USER_ID", maxTurnsDefaultUserID),
		expectedMaxTurns: maxTurnsGetEnvInt("MEMORY_TEST_EXPECTED_MAX_TURNS", 3),
		expectedMaxAge:   maxTurnsGetEnvDuration("MEMORY_TEST_EXPECTED_MAX_AGE", 0),
	}
}

func maxTurnsBuildExactAckPrompt(label string) string {
	return fmt.Sprintf("%s\nStore this exact label for memory diagnostics: %s\nReply with exactly: ACK %s", label, label, label)
}

func maxTurnsSendAndCapture(conn *websocket.Conn, cfg maxTurnsConfig, label string, prompt string) maxTurnsPromptDump {
	knownFiles := maxTurnsListMatchingPromptDumps(cfg.promptDebugPath, cfg.userID)
	resp := maxTurnsSendPrompt(conn, cfg.userID, prompt)
	fmt.Printf("sent %-24s response=%q\n", label, resp.Response)
	return maxTurnsMustReadLatestPromptDump(cfg.promptDebugPath, cfg.userID, knownFiles)
}

func maxTurnsSendPrompt(conn *websocket.Conn, userID string, prompt string) maxTurnsAgentResponse {
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

		var resp maxTurnsAgentResponse
		if err := json.Unmarshal(raw, &resp); err == nil && resp.Model != "" {
			if resp.Error != "" {
				log.Fatalf("Agent returned an error: %s", resp.Error)
			}
			return resp
		}
	}
}

func maxTurnsMustReadLatestPromptDump(dir string, sessionKey string, knownFiles map[string]struct{}) maxTurnsPromptDump {
	pattern := filepath.Join(dir, "prompt_"+maxTurnsSanitizeFilePart(sessionKey, 16)+"_*.md")
	var lastErr error

	for attempt := 0; attempt < maxTurnsDumpPollAttempts; attempt++ {
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
			time.Sleep(maxTurnsDumpPollInterval)
			continue
		}

		raw, err := os.ReadFile(latestPath)
		if err != nil {
			lastErr = err
			time.Sleep(maxTurnsDumpPollInterval)
			continue
		}

		dump, err := maxTurnsParsePromptDump(latestPath, string(raw))
		if err != nil {
			lastErr = err
			time.Sleep(maxTurnsDumpPollInterval)
			continue
		}

		fmt.Printf("reading prompt dump            : %s\n", dump.Path)
		return dump
	}

	log.Fatalf("Failed to read prompt dump for session %q from %s: %v", sessionKey, dir, lastErr)
	return maxTurnsPromptDump{}
}

func maxTurnsListMatchingPromptDumps(dir string, sessionKey string) map[string]struct{} {
	pattern := filepath.Join(dir, "prompt_"+maxTurnsSanitizeFilePart(sessionKey, 16)+"_*.md")
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

func maxTurnsParsePromptDump(path string, raw string) (maxTurnsPromptDump, error) {
	dump := maxTurnsPromptDump{Path: path}
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

		role, ok := maxTurnsFirstBacktickValue(line)
		if !ok {
			return dump, fmt.Errorf("prompt dump %s has malformed message header %q", path, line)
		}

		msg := maxTurnsDumpMessage{Role: role}
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

func maxTurnsFirstBacktickValue(s string) (string, bool) {
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

func maxTurnsHistoryMessages(dump maxTurnsPromptDump) []maxTurnsDumpMessage {
	if len(dump.Messages) <= 2 {
		return nil
	}
	return dump.Messages[1 : len(dump.Messages)-1]
}

func maxTurnsLabelsInMessages(messages []maxTurnsDumpMessage) []string {
	seen := make(map[string]struct{})
	labels := make([]string, 0)
	for _, msg := range messages {
		for _, token := range strings.Fields(msg.Content) {
			trimmed := strings.Trim(token, ".,;:!?()[]{}\"'")
			if !strings.HasPrefix(trimmed, maxTurnsTestPrefix+"-") {
				continue
			}
			if _, ok := seen[trimmed]; ok {
				continue
			}
			seen[trimmed] = struct{}{}
			labels = append(labels, trimmed)
		}
	}
	sort.Strings(labels)
	return labels
}

func maxTurnsAssertExactLabels(actual []string, expected []string, reason string) {
	if len(actual) != len(expected) {
		log.Fatalf("Label mismatch: got=%v want=%v (%s)", actual, expected, reason)
	}
	for i := range actual {
		if actual[i] != expected[i] {
			log.Fatalf("Label mismatch: got=%v want=%v (%s)", actual, expected, reason)
		}
	}
}

func maxTurnsSanitizeFilePart(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) > maxLen {
		runes = runes[:maxLen]
	}
	for i, r := range runes {
		if !maxTurnsIsFileSafe(r) {
			runes[i] = '_'
		}
	}
	return string(runes)
}

func maxTurnsIsFileSafe(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '-' || r == '_'
}

func maxTurnsGetEnv(key string, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}

func maxTurnsGetEnvInt(key string, fallback int) int {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		n, err := strconv.Atoi(value)
		if err == nil {
			return n
		}
	}
	return fallback
}

func maxTurnsGetEnvDuration(key string, fallback time.Duration) time.Duration {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		d, err := time.ParseDuration(value)
		if err == nil {
			return d
		}
	}
	return fallback
}
