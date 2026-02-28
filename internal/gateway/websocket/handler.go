package websocket

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm/ollama"
)

// Upgrades HTTP connection to WebSocket
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type ModelMetrics struct {
	Model              string  `json:"model"`
	TotalDuration      int64   `json:"total_duration_ms"`
	LoadDuration       int64   `json:"load_duration_ms"`
	PromptEvalDuration int64   `json:"prompt_eval_duration_ms"`
	EvalDuration       int64   `json:"eval_duration_ms"`
	TokensPerSecond    float64 `json:"tokens_per_second"`
}

type AgentResponse struct {
	Category      string        `json:"category"`
	Reason        string        `json:"reason"`
	Model         string        `json:"model"`
	Response      string        `json:"response,omitempty"`
	Error         string        `json:"error,omitempty"`
	RouterMetrics *ModelMetrics `json:"router_metrics,omitempty"`
	ExpertMetrics *ModelMetrics `json:"expert_metrics,omitempty"`
}

func mapMetrics(resp *ollama.GenerateResponse) *ModelMetrics {
	if resp == nil || resp.EvalDuration <= 0 {
		return nil
	}

	tps := float64(resp.EvalCount) / (float64(resp.EvalDuration) / 1e9)

	return &ModelMetrics{
		Model:              resp.Model,
		TotalDuration:      resp.TotalDuration / 1e6,
		LoadDuration:       resp.LoadDuration / 1e6,
		PromptEvalDuration: resp.PromptEvalDuration / 1e6,
		EvalDuration:       resp.EvalDuration / 1e6,
		TokensPerSecond:    tps,
	}
}

func HandleConnections(w http.ResponseWriter, r *http.Request, client *ollama.Client, cfg *config.Config) {
	// Upgrade init request to Websocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrader error:", err)
		return
	}
	// Close if err
	defer conn.Close()

	// Loop for read and write
	for {
		// Read
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Println("Read error", err)
			break
		}

		// Print message
		userPrompt := string(message)

		// Create a context with a strict timeout for the fast router model
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		// Ask the triage model to classify the prompt
		decision, err := agent.DetermineRoute(ctx, client, cfg.OllamaRouterModel, userPrompt)
		cancel()

		if err != nil {
			log.Println("Routing error:", err)
			conn.WriteMessage(messageType, []byte("Error: Failed to route the request."))
			continue
		}

		expertModel, systemPrompt := decision.GetRouteDetails(cfg)

		// Give the expert model a much longer timeout to generate the full response
		genCtx, genCancel := context.WithTimeout(context.Background(), 3*time.Minute)

		expertReq := ollama.GenerateRequest{
			Model:  expertModel,
			Prompt: userPrompt,
			System: systemPrompt,
			Stream: false, // Still false, so we wait for the entire markdown response
		}

		// Execute the prompt against the selected expert model
		expertResp, err := client.Generate(genCtx, expertReq)
		genCancel() // Immediately free context resources after generation

		if err != nil {
			log.Println("Expert generation error:", err)
			errPayload, _ := json.Marshal(AgentResponse{
				Category: decision.Category,
				Model:    expertModel,
				Error:    fmt.Sprintf("Oswald's %s model failed to respond: %v", expertModel, err),
			})
			conn.WriteMessage(messageType, errPayload)
			continue
		}

		finalPayload := AgentResponse{
			Category:      decision.Category,
			Reason:        decision.Reason,
			Response:      expertResp.Response,
			RouterMetrics: mapMetrics(decision.Metrics),
			ExpertMetrics: mapMetrics(expertResp),
		}

		// Marshal the struct into a JSON byte array
		jsonBytes, err := json.Marshal(finalPayload)
		if err != nil {
			log.Println("Failed to marshal JSON payload:", err)
			continue
		}

		// Return the structured JSON to the client
		err = conn.WriteMessage(messageType, jsonBytes)
		if err != nil {
			log.Println("Write error:", err)
			break
		}
	}
}
