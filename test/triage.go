package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"

	"github.com/gorilla/websocket"
)

// AgentResponse mirrors internal/agent/agent.go's AgentResponse for response validation.
// This avoids importing internal types.
type AgentResponse struct {
	Model    string `json:"model"`
	Response string `json:"response"`
	Error    string `json:"error"`
	Metrics  *struct {
		Model string `json:"model"`
	} `json:"metrics"`
}

// TestCase defines a pipeline test: a label and the prompt to send.
type TestCase struct {
	Name   string // Test label
	Prompt string // User prompt
}

// main runs pipeline tests against the WebSocket gateway.
// Connects to ws://localhost:8080/ws and validates that each prompt produces
// a non-empty response without errors.
func main() {
	port := "8080"
	u := url.URL{Scheme: "ws", Host: "localhost:" + port, Path: "/ws"}

	fmt.Printf("Starting Oswald AI Pipeline Tests against %s...\n", u.String())
	fmt.Println("--------------------------------------------------------")

	tests := []TestCase{
		{Name: "CURRENT_EVENT", Prompt: "Who won the most recent Super Bowl?"},
		{Name: "NEWS", Prompt: "What are the latest developments in AI regulation?"},
		{Name: "FACTUAL", Prompt: "What is the current price of Bitcoin?"},
		{Name: "RECENT_TECH", Prompt: "What are the new features in the latest Go release?"},
		{Name: "WEATHER", Prompt: "What is the weather in New York today?"},
		{Name: "CONVERSATIONAL", Prompt: "Hello, how are you?"},
		{Name: "OPINION", Prompt: "What is your favorite color?"},
		{Name: "MATH", Prompt: "What is 42 times 7?"},
		{Name: "GENERAL_KNOWLEDGE", Prompt: "What is the capital of France?"},
		{Name: "CODING", Prompt: "Write a Go function that reverses a string."},
	}

	passed := 0
	failed := 0

	for _, tc := range tests {
		conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
		if err != nil {
			log.Fatalf("Failed to connect to %s: %v", u.String(), err)
		}

		err = conn.WriteMessage(websocket.TextMessage, []byte(tc.Prompt))
		if err != nil {
			log.Printf("[%s] Write error: %v", tc.Name, err)
			conn.Close()
			continue
		}

		var resp AgentResponse

		// Read streaming chunks until the final JSON payload arrives
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				log.Printf("[%s] Read error: %v", tc.Name, err)
				break
			}

			err = json.Unmarshal(message, &resp)

			// Final payload has a non-empty model field
			if err == nil && resp.Model != "" {
				break
			}
		}

		conn.Close()

		if resp.Error != "" {
			fmt.Printf("FAIL  | [%s] Agent error: %s\n", tc.Name, resp.Error)
			failed++
			continue
		}

		if resp.Response != "" {
			fmt.Printf("PASS  | [%s] got response (%d chars, model: %s)\n", tc.Name, len(resp.Response), resp.Model)
			passed++
		} else {
			fmt.Printf("FAIL  | [%s] empty response (model: %s)\n", tc.Name, resp.Model)
			failed++
		}
	}

	fmt.Println("--------------------------------------------------------")
	fmt.Printf("Test Run Complete: %d Passed, %d Failed.\n", passed, failed)
}
