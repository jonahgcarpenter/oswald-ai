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
	ttlDefaultPort           = "8080"
	ttlDefaultAgentTracePath = "./tmp/agent-trace"
	ttlDefaultUserID         = "memory-test-ttl"
	ttlTestPrefix            = "MEMTEST"
	ttlDumpPollInterval      = 200 * time.Millisecond
	ttlDumpPollAttempts      = 50
	ttlSleepPad              = 1500 * time.Millisecond
)

type ttlConfig struct {
	port           string
	agentTracePath string
	userID         string
	expectedMaxAge time.Duration
}

type ttlAgentResponse struct {
	Model    string `json:"model"`
	Response string `json:"response"`
	Error    string `json:"error"`
}

type ttlPromptDump struct {
	Path     string
	Messages []ttlDumpMessage
}

type ttlDumpMessage struct {
	Role    string
	Content string
}

func main() {
	cfg := ttlLoadConfig()
	if cfg.expectedMaxAge <= 0 {
		log.Fatalf("This TTL test requires MEMORY_MAX_AGE > 0. Current expected value: %s", cfg.expectedMaxAge)
	}

	fmt.Println("Memory TTL integration test")
	fmt.Println("--------------------------------------------------------------------------------")
	fmt.Printf("WebSocket endpoint              : ws://localhost:%s/ws\n", cfg.port)
	fmt.Printf("Agent trace directory           : %s\n", cfg.agentTracePath)
	fmt.Printf("Expected server MEMORY_MAX_AGE  : %s\n", cfg.expectedMaxAge)
	fmt.Printf("Expected server AGENT_TRACE_PATH: %s\n", cfg.agentTracePath)
	fmt.Println("This test expects a low non-zero TTL and validates expiration from Markdown agent traces.")
	fmt.Println("--------------------------------------------------------------------------------")

	u := url.URL{Scheme: "ws", Host: "localhost:" + cfg.port, Path: "/ws"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Fatalf("Failed to connect to %s: %v", u.String(), err)
	}
	defer conn.Close()

	seedLabels := []string{ttlTestPrefix + "-TTL-01", ttlTestPrefix + "-TTL-02"}
	for _, label := range seedLabels {
		ttlSendAndCapture(conn, cfg, label, ttlBuildExactAckPrompt(label))
	}

	waitFor := cfg.expectedMaxAge + ttlSleepPad
	fmt.Printf("Sleeping %s so retained history exceeds MEMORY_MAX_AGE=%s\n", waitFor.Round(time.Millisecond), cfg.expectedMaxAge)
	time.Sleep(waitFor)

	triggerLabel := ttlTestPrefix + "-TTL-TRIGGER"
	dump := ttlSendAndCapture(conn, cfg, triggerLabel, ttlBuildExactAckPrompt(triggerLabel))
	history := ttlHistoryMessages(dump)
	ttlAssertNotContainsAny(history, seedLabels, "TTL should prune older turn pairs before the next request")

	fmt.Printf("TTL validation passed. Dump: %s\n", dump.Path)
}

func ttlLoadConfig() ttlConfig {
	return ttlConfig{
		port:           ttlGetEnv("MEMORY_TEST_PORT", ttlDefaultPort),
		agentTracePath: ttlGetEnv("MEMORY_TEST_AGENT_TRACE_PATH", ttlDefaultAgentTracePath),
		userID:         ttlGetEnv("MEMORY_TEST_USER_ID", ttlDefaultUserID),
		expectedMaxAge: ttlGetEnvDuration("MEMORY_TEST_EXPECTED_MAX_AGE", 5*time.Second),
	}
}

func ttlBuildExactAckPrompt(label string) string {
	return fmt.Sprintf("%s\nStore this exact label for memory diagnostics: %s\nReply with exactly: ACK %s", label, label, label)
}

func ttlSendAndCapture(conn *websocket.Conn, cfg ttlConfig, label string, prompt string) ttlPromptDump {
	knownFiles := ttlListMatchingAgentTraces(cfg.agentTracePath, cfg.userID)
	resp := ttlSendPrompt(conn, cfg.userID, prompt)
	fmt.Printf("sent %-24s response=%q\n", label, resp.Response)
	return ttlMustReadLatestAgentTrace(cfg.agentTracePath, cfg.userID, knownFiles)
}

func ttlSendPrompt(conn *websocket.Conn, userID string, prompt string) ttlAgentResponse {
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

		var resp ttlAgentResponse
		if err := json.Unmarshal(raw, &resp); err == nil && resp.Model != "" {
			if resp.Error != "" {
				log.Fatalf("Agent returned an error: %s", resp.Error)
			}
			return resp
		}
	}
}

func ttlMustReadLatestAgentTrace(dir string, sessionKey string, knownFiles map[string]struct{}) ttlPromptDump {
	pattern := filepath.Join(dir, "trace_"+ttlSanitizeFilePart(sessionKey, 16)+"_*.md")
	var lastErr error

	for attempt := 0; attempt < ttlDumpPollAttempts; attempt++ {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			log.Fatalf("Invalid agent trace glob %q: %v", pattern, err)
		}

		allFiles, err := filepath.Glob(filepath.Join(dir, "*.md"))
		if err != nil {
			log.Fatalf("Invalid agent trace directory glob for %q: %v", dir, err)
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
				lastErr = fmt.Errorf("no markdown agent traces found in %s; ensure the server is running with AGENT_TRACE_PATH=%s and restart it", dir, dir)
			} else {
				lastErr = fmt.Errorf("no new agent trace found for session %q (pattern %s)", sessionKey, pattern)
			}
			time.Sleep(ttlDumpPollInterval)
			continue
		}

		raw, err := os.ReadFile(latestPath)
		if err != nil {
			lastErr = err
			time.Sleep(ttlDumpPollInterval)
			continue
		}

		dump, err := ttlParsePromptDump(latestPath, string(raw))
		if err != nil {
			lastErr = err
			time.Sleep(ttlDumpPollInterval)
			continue
		}

		fmt.Printf("reading agent trace           : %s\n", dump.Path)
		return dump
	}

	log.Fatalf("Failed to read agent trace for session %q from %s: %v", sessionKey, dir, lastErr)
	return ttlPromptDump{}
}

func ttlListMatchingAgentTraces(dir string, sessionKey string) map[string]struct{} {
	pattern := filepath.Join(dir, "trace_"+ttlSanitizeFilePart(sessionKey, 16)+"_*.md")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		log.Fatalf("Invalid agent trace glob %q: %v", pattern, err)
	}
	seen := make(map[string]struct{}, len(matches))
	for _, path := range matches {
		seen[path] = struct{}{}
	}
	return seen
}

func ttlParsePromptDump(path string, raw string) (ttlPromptDump, error) {
	dump := ttlPromptDump{Path: path}
	sectionStart := strings.Index(raw, "## Full Message Transcript")
	if sectionStart < 0 {
		return dump, fmt.Errorf("agent trace %s missing transcript section", path)
	}

	lines := strings.Split(raw[sectionStart:], "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if !strings.HasPrefix(line, "### [") {
			continue
		}

		role, ok := ttlFirstBacktickValue(line)
		if !ok {
			return dump, fmt.Errorf("agent trace %s has malformed message header %q", path, line)
		}

		msg := ttlDumpMessage{Role: role}
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
		return dump, fmt.Errorf("agent trace %s contained no parsed messages", path)
	}
	return dump, nil
}

func ttlFirstBacktickValue(s string) (string, bool) {
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

func ttlHistoryMessages(dump ttlPromptDump) []ttlDumpMessage {
	if len(dump.Messages) <= 2 {
		return nil
	}
	return dump.Messages[1 : len(dump.Messages)-1]
}

func ttlAssertNotContainsAny(messages []ttlDumpMessage, labels []string, reason string) {
	for _, msg := range messages {
		for _, label := range labels {
			if strings.Contains(msg.Content, label) {
				log.Fatalf("Unexpected retained label %q: %s", label, reason)
			}
		}
	}
}

func ttlSanitizeFilePart(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) > maxLen {
		runes = runes[:maxLen]
	}
	for i, r := range runes {
		if !ttlIsFileSafe(r) {
			runes[i] = '_'
		}
	}
	return string(runes)
}

func ttlIsFileSafe(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '-' || r == '_'
}

func ttlGetEnv(key string, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}

func ttlGetEnvDuration(key string, fallback time.Duration) time.Duration {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		d, err := time.ParseDuration(value)
		if err == nil {
			return d
		}
	}
	return fallback
}

func ttlGetEnvInt(key string, fallback int) int {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		n, err := strconv.Atoi(value)
		if err == nil {
			return n
		}
	}
	return fallback
}
