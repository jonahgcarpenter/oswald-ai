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
	Model         string `json:"model"`
	Response      string `json:"response"`
	SearchSummary string `json:"search_summary"`
	Error         string `json:"error"`
	Metrics       *struct {
		Model string `json:"model"`
	} `json:"metrics"`
}

// TestCase defines a search pipeline test: whether a web search is expected and the prompt.
type TestCase struct {
	Name         string // Test label
	Prompt       string // User prompt
	ExpectSearch bool   // Whether we expect a non-empty search_summary in the response
}

// main runs search pipeline tests against the WebSocket gateway.
// Connects to ws://localhost:8080/ws and validates that prompts requiring
// web search produce a non-empty search_summary, and conversational prompts do not.
func main() {
	port := "8080"
	u := url.URL{Scheme: "ws", Host: "localhost:" + port, Path: "/ws"}

	fmt.Printf("Starting Oswald AI Search Pipeline Tests against %s...\n", u.String())
	fmt.Println("--------------------------------------------------------")

	tests := []TestCase{
		// Prompts that should trigger web search
		{Name: "CURRENT_EVENT", Prompt: "Who won the most recent Super Bowl?", ExpectSearch: true},
		{Name: "NEWS", Prompt: "What are the latest developments in AI regulation?", ExpectSearch: true},
		{Name: "FACTUAL", Prompt: "What is the current price of Bitcoin?", ExpectSearch: true},
		{Name: "RECENT_TECH", Prompt: "What are the new features in the latest Go release?", ExpectSearch: true},
		{Name: "WEATHER", Prompt: "What is the weather in New York today?", ExpectSearch: true},

		// Prompts that should NOT trigger web search
		{Name: "CONVERSATIONAL", Prompt: "Hello, how are you?", ExpectSearch: false},
		{Name: "OPINION", Prompt: "What is your favorite color?", ExpectSearch: false},
		{Name: "MATH", Prompt: "What is 42 times 7?", ExpectSearch: false},
		{Name: "GENERAL_KNOWLEDGE", Prompt: "What is the capital of France?", ExpectSearch: false},
		{Name: "CODING", Prompt: "Write a Go function that reverses a string.", ExpectSearch: false},
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
			fmt.Printf("ERROR | [%s] Agent error: %s\n", tc.Name, resp.Error)
			failed++
			continue
		}

		gotSearch := resp.SearchSummary != ""
		if gotSearch == tc.ExpectSearch {
			searchLabel := "no search"
			if gotSearch {
				searchLabel = fmt.Sprintf("searched (%d chars summary)", len(resp.SearchSummary))
			}
			fmt.Printf("PASS  | [%s] %s (model: %s)\n", tc.Name, searchLabel, resp.Model)
			passed++
		} else {
			expected := "no search"
			got := "no search"
			if tc.ExpectSearch {
				expected = "search"
			}
			if gotSearch {
				got = "search"
			}
			fmt.Printf("FAIL  | [%s] expected=%s got=%s prompt=%q\n", tc.Name, expected, got, tc.Prompt)
			failed++
		}
	}

	fmt.Println("--------------------------------------------------------")
	fmt.Printf("Test Run Complete: %d Passed, %d Failed.\n", passed, failed)
}
