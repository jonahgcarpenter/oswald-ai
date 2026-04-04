package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/gorilla/websocket"
)

// ANSI color codes
const (
	colorReset  = "\033[0m"
	colorDim    = "\033[2m"
	colorYellow = "\033[33m"
	colorGray   = "\033[90m"
)

// interactiveStreamChunk mirrors agent.StreamChunk for decoding streamed messages.
type interactiveStreamChunk struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// interactiveAgentResponse mirrors agent.AgentResponse for decoding the final payload.
type interactiveAgentResponse struct {
	Model    string `json:"model"`
	Response string `json:"response"`
	Thinking string `json:"thinking"`
	Error    string `json:"error"`
	Metrics  *struct {
		Model           string  `json:"model"`
		TotalDuration   int64   `json:"total_duration_ms"`
		TokensPerSecond float64 `json:"tokens_per_second"`
	} `json:"metrics"`
}

// isStreamChunk returns true when the message looks like a StreamChunk
// (has a "type" field but no "model" field).
func isStreamChunk(raw []byte) (interactiveStreamChunk, bool) {
	var chunk interactiveStreamChunk
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return chunk, false
	}
	return chunk, chunk.Type != ""
}

// isFinalPayload returns true when the message is a completed AgentResponse
// (has a non-empty "model" field).
func isFinalPayload(raw []byte) (interactiveAgentResponse, bool) {
	var resp interactiveAgentResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return resp, false
	}
	return resp, resp.Model != ""
}

// connect establishes a WebSocket connection to the given URL, retrying on failure.
func connect(u url.URL) *websocket.Conn {
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Fatalf("Failed to connect to %s: %v", u.String(), err)
	}
	return conn
}

// main is the entry point for the interactive Oswald AI terminal client.
// Connects to the WebSocket gateway, reads user prompts from stdin, and
// streams the model's response with color-coded thinking, content, and status chunks.
func main() {
	port := "8080"
	userID := "interactive-test-user"
	u := url.URL{Scheme: "ws", Host: "localhost:" + port, Path: "/ws"}

	fmt.Printf("Oswald AI Interactive Client (%s)\n", u.String())
	fmt.Printf("User ID: %s\n", userID)
	fmt.Println("Type your message and press Enter. Ctrl+C to quit.")
	fmt.Println()

	conn := connect(u)
	defer conn.Close()

	// Handle Ctrl+C / SIGTERM for a clean exit
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		fmt.Println()
		conn.Close()
		os.Exit(0)
	}()

	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("> ")

		if !scanner.Scan() {
			// EOF (e.g. piped input exhausted)
			break
		}

		prompt := scanner.Text()
		if prompt == "" {
			continue
		}

		msg, err := json.Marshal(struct {
			UserID string `json:"user_id"`
			Prompt string `json:"prompt"`
		}{UserID: userID, Prompt: prompt})
		if err != nil {
			fmt.Printf("\n%s[error] Failed to marshal message: %v%s\n", colorGray, err, colorReset)
			continue
		}
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			fmt.Printf("\n%s[error] Failed to send message: %v%s\n", colorGray, err, colorReset)
			// Reconnect and retry
			conn.Close()
			conn = connect(u)
			continue
		}

		// Stream the response
		inThinking := false
		inContent := false

		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				fmt.Printf("\n%s[error] Connection lost: %v%s\n\n", colorGray, err, colorReset)
				conn = connect(u)
				break
			}

			// Check if this is a final AgentResponse payload
			if resp, ok := isFinalPayload(raw); ok {
				// Ensure we're on a fresh line after streamed content
				if inThinking || inContent {
					fmt.Print(colorReset)
					fmt.Println()
				}
				fmt.Println()

				if resp.Error != "" {
					fmt.Printf("%s[error] %s%s\n\n", colorGray, resp.Error, colorReset)
					break
				}

				// Compact metrics summary
				if resp.Metrics != nil {
					thinkingLen := len(resp.Thinking)
					fmt.Printf("%s  model=%s | %.1f tok/s | %dms",
						colorGray,
						resp.Metrics.Model,
						resp.Metrics.TokensPerSecond,
						resp.Metrics.TotalDuration,
					)
					if thinkingLen > 0 {
						// Estimate thinking tokens from character count (~4 chars/token)
						fmt.Printf(" | thinking: ~%d tokens", thinkingLen/4)
					}
					fmt.Printf("%s\n\n", colorReset)
				}

				inThinking = false
				inContent = false
				break
			}

			// Otherwise try to decode as a StreamChunk
			chunk, ok := isStreamChunk(raw)
			if !ok {
				continue
			}

			switch chunk.Type {
			case "thinking":
				if !inThinking {
					// Print the thinking prefix on first thinking chunk
					fmt.Printf("%s[thinking] ", colorDim)
					inThinking = true
				}
				fmt.Print(chunk.Text)

			case "content":
				if inThinking {
					// Transition: end thinking block, start content on a new line
					fmt.Print(colorReset)
					fmt.Println()
					fmt.Println()
					inThinking = false
				}
				if !inContent {
					fmt.Print(colorReset)
					inContent = true
				}
				fmt.Print(chunk.Text)

			case "status":
				if inThinking {
					// End any active thinking block cleanly
					fmt.Print(colorReset)
					fmt.Println()
					inThinking = false
				}
				fmt.Printf("\n%s%s%s\n", colorYellow, chunk.Text, colorReset)
			}
		}
	}
}
