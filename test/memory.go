package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	defaultPort     = "8080"
	defaultDumpPath = "./tmp/memory-debug.json"
	defaultUserID   = "memory-test-user"
	testPrefix      = "MEMTEST"

	maxTurnPhaseCount = 5
	contextPhaseCount = 3
	contextBlobRepeat = 180
	contextSleepPad   = 1500 * time.Millisecond
)

type memoryAgentResponse struct {
	Model    string `json:"model"`
	Response string `json:"response"`
	Error    string `json:"error"`
}

type memoryDebugFile struct {
	Memory memorySnapshot `json:"memory"`
}

type memorySnapshot struct {
	GeneratedAt string                           `json:"generated_at"`
	Retention   memorySnapshotRetention          `json:"retention"`
	Context     memorySnapshotContext            `json:"context"`
	Sessions    map[string]memorySnapshotSession `json:"sessions"`
}

type memorySnapshotRetention struct {
	MaxTurns int    `json:"max_turns"`
	MaxAge   string `json:"max_age"`
}

type memorySnapshotContext struct {
	ContextWindow int `json:"context_window"`
	PromptBudget  int `json:"prompt_budget"`
}

type memorySnapshotSession struct {
	PromptEstimate memoryPromptEstimate `json:"prompt_estimate"`
	Turns          []memorySnapshotTurn `json:"turns"`
}

type memoryPromptEstimate struct {
	EstimatedBefore int `json:"estimated_before"`
	EstimatedAfter  int `json:"estimated_after"`
}

type memorySnapshotTurn struct {
	CreatedAt string                `json:"created_at"`
	User      memorySnapshotMessage `json:"user"`
	Assistant memorySnapshotMessage `json:"assistant"`
}

type memorySnapshotMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func main() {
	port := getEnv("MEMORY_TEST_PORT", defaultPort)
	dumpPath := getEnv("MEMORY_TEST_DUMP_PATH", defaultDumpPath)
	userID := getEnv("MEMORY_TEST_USER_ID", defaultUserID)
	u := url.URL{Scheme: "ws", Host: "localhost:" + port, Path: "/ws"}

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Fatalf("Failed to connect to %s: %v", u.String(), err)
	}
	defer conn.Close()

	fmt.Printf("Running memory integration test against %s\n", u.String())
	fmt.Printf("Reading debug dump from %s\n", dumpPath)
	fmt.Printf("Using user ID: %s\n\n", userID)

	maxTurnsLabels := runMaxTurnPhase(conn, dumpPath, userID)
	ttlTriggerLabel := runTTLPhase(conn, dumpPath, userID)
	runContextPhase(conn, dumpPath, userID)

	finalDump := mustReadDump(dumpPath)
	sessionKey, session := selectTestSession(finalDump.Memory)
	labels := sessionLabels(session)

	fmt.Println("Final snapshot")
	fmt.Println("--------------------------------------------------------------------------------")
	fmt.Printf("session           : %s\n", sessionKey)
	fmt.Printf("retained labels   : %s\n", strings.Join(labels, ", "))
	fmt.Printf("estimated before  : %d\n", session.PromptEstimate.EstimatedBefore)
	fmt.Printf("estimated after   : %d\n", session.PromptEstimate.EstimatedAfter)
	fmt.Printf("max-turn labels   : %s\n", strings.Join(maxTurnsLabels, ", "))
	fmt.Printf("ttl trigger label : %s\n", ttlTriggerLabel)
	fmt.Println("--------------------------------------------------------------------------------")
	fmt.Println("Inspect ./tmp/memory-debug.json for full state and turn contents.")
}

func runMaxTurnPhase(conn *websocket.Conn, dumpPath string, userID string) []string {
	fmt.Println("Phase 1 - max turn retention")
	labels := make([]string, 0, maxTurnPhaseCount)
	for i := 1; i <= maxTurnPhaseCount; i++ {
		label := fmt.Sprintf("%s-MAX-%02d", testPrefix, i)
		labels = append(labels, label)
		prompt := fmt.Sprintf("%s\nStore this exact label for memory testing: %s\nReply with exactly: ACK %s", label, label, label)
		resp := sendPrompt(conn, userID, prompt)
		fmt.Printf("sent %-16s response=%q\n", label, resp.Response)
	}

	dump := mustReadDump(dumpPath)
	_, session := selectTestSession(dump.Memory)
	retained := sessionLabels(session)
	fmt.Printf("configured max_turns : %d\n", dump.Memory.Retention.MaxTurns)
	fmt.Printf("retained labels      : %s\n\n", strings.Join(retained, ", "))
	return labels
}

func runTTLPhase(conn *websocket.Conn, dumpPath string, userID string) string {
	fmt.Println("Phase 2 - TTL retention")
	dump := mustReadDump(dumpPath)
	maxAge, err := time.ParseDuration(dump.Memory.Retention.MaxAge)
	if err != nil {
		log.Fatalf("Failed to parse max age %q from dump: %v", dump.Memory.Retention.MaxAge, err)
	}
	if maxAge <= 0 {
		log.Fatalf("TTL phase requires MEMORY_MAX_AGE > 0, got %q", dump.Memory.Retention.MaxAge)
	}

	label := fmt.Sprintf("%s-TTL-TRIGGER", testPrefix)
	waitFor := maxAge + contextSleepPad
	fmt.Printf("sleeping %s to exceed TTL %s\n", waitFor.Round(time.Millisecond), maxAge)
	time.Sleep(waitFor)

	prompt := fmt.Sprintf("%s\nThis prompt should trigger TTL pruning. Reply with exactly: ACK %s", label, label)
	resp := sendPrompt(conn, userID, prompt)
	fmt.Printf("sent %-16s response=%q\n", label, resp.Response)

	dump = mustReadDump(dumpPath)
	_, session := selectTestSession(dump.Memory)
	retained := sessionLabels(session)
	fmt.Printf("retained labels      : %s\n\n", strings.Join(retained, ", "))
	return label
}

func runContextPhase(conn *websocket.Conn, dumpPath string, userID string) {
	fmt.Println("Phase 3 - context-budget pruning")
	for i := 1; i <= contextPhaseCount; i++ {
		label := fmt.Sprintf("%s-CTX-%02d", testPrefix, i)
		prompt := buildLargeContextPrompt(label)
		resp := sendPrompt(conn, userID, prompt)
		fmt.Printf("sent %-16s response=%q\n", label, resp.Response)
	}

	dump := mustReadDump(dumpPath)
	_, session := selectTestSession(dump.Memory)
	retained := sessionLabels(session)
	fmt.Printf("context_window       : %d\n", dump.Memory.Context.ContextWindow)
	fmt.Printf("prompt_budget        : %d\n", dump.Memory.Context.PromptBudget)
	fmt.Printf("estimated before     : %d\n", session.PromptEstimate.EstimatedBefore)
	fmt.Printf("estimated after      : %d\n", session.PromptEstimate.EstimatedAfter)
	fmt.Printf("retained labels      : %s\n\n", strings.Join(retained, ", "))
}

func buildLargeContextPrompt(label string) string {
	chunk := fmt.Sprintf("%s payload segment for context pruning validation. ", label)
	largeBody := strings.Repeat(chunk, contextBlobRepeat)
	return fmt.Sprintf("%s\nRemember this exact label for context pruning diagnostics: %s\n%s\nReply with exactly: ACK %s", label, label, largeBody, label)
}

func sendPrompt(conn *websocket.Conn, userID string, prompt string) memoryAgentResponse {
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

		var resp memoryAgentResponse
		if err := json.Unmarshal(raw, &resp); err == nil && resp.Model != "" {
			if resp.Error != "" {
				log.Fatalf("Agent returned an error: %s", resp.Error)
			}
			return resp
		}
	}
}

func mustReadDump(path string) memoryDebugFile {
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		raw, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}

		var dump memoryDebugFile
		if err := json.Unmarshal(raw, &dump); err != nil {
			lastErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if len(dump.Memory.Sessions) == 0 {
			lastErr = fmt.Errorf("memory dump contains no sessions yet")
			time.Sleep(200 * time.Millisecond)
			continue
		}

		return dump
	}

	log.Fatalf("Failed to read memory dump %s: %v", path, lastErr)
	return memoryDebugFile{}
}

func selectTestSession(memory memorySnapshot) (string, memorySnapshotSession) {
	type scoredSession struct {
		key     string
		session memorySnapshotSession
		score   int
	}

	best := scoredSession{score: -1}
	for key, session := range memory.Sessions {
		score := 0
		for _, turn := range session.Turns {
			if strings.Contains(turn.User.Content, testPrefix) {
				score++
			}
		}
		if score > best.score {
			best = scoredSession{key: key, session: session, score: score}
		}
	}

	if best.score <= 0 {
		log.Fatal("Could not find a session in the debug dump containing memory test labels")
	}

	return best.key, best.session
}

func sessionLabels(session memorySnapshotSession) []string {
	labels := make([]string, 0, len(session.Turns))
	seen := make(map[string]struct{})
	for _, turn := range session.Turns {
		for _, token := range strings.Fields(turn.User.Content) {
			trimmed := strings.Trim(token, ".,;:!?()[]{}\"'")
			if !strings.HasPrefix(trimmed, testPrefix+"-") {
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

func getEnv(key string, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}
