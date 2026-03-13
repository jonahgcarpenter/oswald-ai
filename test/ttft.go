package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

// AgentResponse mirrors the struct to extract metrics
type AgentResponse struct {
	Category      string `json:"category"`
	Error         string `json:"error"`
	ExpertMetrics *struct {
		Model              string `json:"model"`
		PromptEvalDuration int64  `json:"prompt_eval_duration_ms"`
		EvalDuration       int64  `json:"eval_duration_ms"`
	} `json:"expert_metrics"`
}

type TestCase struct {
	Name   string
	Prompt string
}

/*
 * This test is designed to determine the Time to First Token (TTFT)
 * and test the streaming capabilities of the WebSocket gateway.
 */
func main() {
	port := "8080"
	u := url.URL{Scheme: "ws", Host: "localhost:" + port, Path: "/ws"}

	fmt.Printf("Starting Oswald TTFT (Time To First Token) Streaming Benchmark against %s...\n", u.String())
	fmt.Println("--------------------------------------------------------------------------------")

	tests := []TestCase{
		{
			Name:   "SHORT",
			Prompt: "Hello.",
		},
		{
			Name:   "MEDIUM",
			Prompt: "Explain the difference between a mutex and a channel in Go, and provide a small code example of when to use each.",
		},
		{
			Name:   "LONG",
			Prompt: "I am migrating a bare-metal environment from Proxmox to a Talos Linux and k3s cluster. Compare the pros and cons of this migration. Consider storage solutions like Longhorn, network policies using Cilium, and how to properly expose internal services using Nginx Proxy Manager and Cloudflare tunnels. Provide a step-by-step architectural breakdown of how you would design this stack for maximum uptime and minimal resource overhead.",
		},
	}

	for _, tc := range tests {
		fmt.Printf("Running [%s] Prompt Benchmark...\n", tc.Name)

		// 1. Establish connection
		conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
		if err != nil {
			log.Fatalf("Failed to connect: %v", err)
		}

		// 2. Start the timer and send the prompt
		startTime := time.Now()
		err = conn.WriteMessage(websocket.TextMessage, []byte(tc.Prompt))
		if err != nil {
			log.Printf("Write error: %v", err)
			conn.Close()
			continue
		}

		var networkTTFT time.Duration
		firstTokenReceived := false
		var finalResp AgentResponse

		// 3. Loop to receive streaming chunks
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				log.Printf("Read error: %v", err)
				break
			}

			// The moment we get the very first message back, stop the TTFT timer
			if !firstTokenReceived {
				networkTTFT = time.Since(startTime)
				firstTokenReceived = true
			}

			// Try to parse the message as the final JSON payload
			err = json.Unmarshal(message, &finalResp)

			// If it parses successfully and has a Category, we know it's the final payload, not a text chunk
			if err == nil && finalResp.Category != "" {
				break
			}

			// Optional: If you want to see the stream in real-time in your terminal, uncomment the line below:
			// fmt.Print(string(message))
		}

		// 4. Stop the total generation timer
		totalNetworkWaitTime := time.Since(startTime)

		if finalResp.Error != "" {
			fmt.Printf(" ↳ Agent returned an error: %s\n", finalResp.Error)
			conn.Close()
			continue
		}

		// Safely extract metrics if they exist
		var modelTTFT int64 = 0
		var modelName = "unknown"
		if finalResp.ExpertMetrics != nil {
			modelTTFT = finalResp.ExpertMetrics.PromptEvalDuration
			modelName = finalResp.ExpertMetrics.Model
		}

		// Print Results
		fmt.Printf(" ↳ Model Used          : %s\n", modelName)
		fmt.Printf(" ↳ Prompt Length       : %d characters\n", len(tc.Prompt))
		fmt.Printf(" ↳ Network TTFT        : %v (Time to receive first streamed chunk)\n", networkTTFT)
		fmt.Printf(" ↳ True Model TTFT     : %d ms (Ollama Prompt Eval Time)\n", modelTTFT)
		fmt.Printf(" ↳ Total Network Wait  : %v (Time for full streamed generation)\n", totalNetworkWaitTime)
		fmt.Println("--------------------------------------------------------------------------------")

		conn.Close()

		// Brief pause between tests to let the GPU breathe
		time.Sleep(2 * time.Second)
	}

	fmt.Println("Benchmark Complete.")
}
