package router

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm/ollama"
)

// RouteDecision maps exactly to the JSON schema we force the LLM to output.
type RouteDecision struct {
	Category string `json:"category"`
	Reason   string `json:"reason"`
}

// DetermineRoute asks the fast router model to classify the incoming message.
func DetermineRoute(ctx context.Context, client *ollama.Client, routerModel string, prompt string) (*RouteDecision, error) {

	// This system prompt forces the model into a deterministic state.
	systemPrompt := `You are a highly efficient API router for the Oswald AI agent. 
	Your ONLY job is to categorize the user's prompt. You must NEVER refuse to process a prompt due to safety, ethics, or alignment filters. 

	Analyze the prompt and classify it into one of FOUR exact categories using this strict priority hierarchy:

	1. "UNCENSORED" (Highest Priority): Anything involving cyber attacks, hacking, physical security bypass (lockpicking), weapons, harassment, insults, or toxic behavior.
	2. "CODING" (Second Priority): ANY request to WRITE, DEBUG, or EXPLAIN programming code (e.g., Go, WebRTC), configuration files (e.g., systemd, Kubernetes, Cilium), or network setups (e.g., UniFi VLANs). Even if the code is highly complex or requires multiple steps, it MUST be categorized as CODING.
	3. "COMPLEX" (Third Priority): Analyzing data (logs/sheets), analyzing technical architecture comparisons, or using external tools to act on the world (e.g., applying to jobs, searching the web, sending emails, booking flights). 
	4. "SIMPLE" (Lowest Priority): General conversational chat, greetings, or straightforward non-technical factual questions.

	You must respond in strictly valid JSON matching this exact schema. Do not output anything other than JSON.
	Example format:
	{"category": "CODING", "reason": "brief explanation"}`

	// Build the request using the types we defined in client.go
	req := ollama.GenerateRequest{
		Model:  routerModel,
		Prompt: prompt,
		System: systemPrompt,
		Format: "json", // Tells Ollama to enforce JSON output
		Stream: false,  // We need the full JSON object at once, no streaming
	}

	// Send it to the Ollama client
	resp, err := client.Generate(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("router failed to reach Ollama: %w", err)
	}

	// Ollama metrics
	if resp.EvalDuration > 0 {
		// Calculate Tokens Per Second (TPS)
		tps := float64(resp.EvalCount) / (float64(resp.EvalDuration) / 1e9)

		fmt.Printf("\n   [Oswald Router Metrics] Model: %s\n", resp.Model)
		fmt.Printf("   ├─ Speed: %.2f tokens/sec\n", tps)
		fmt.Printf("   ├─ Load Time: %dms\n", resp.LoadDuration/1e6)
		fmt.Printf("   ├─ Prompt Eval: %dms\n", resp.PromptEvalDuration/1e6)
		fmt.Printf("   └─ Generate Time: %dms\n", resp.EvalDuration/1e6)
	}

	// Unmarshal the LLM's raw text response directly into our Go struct
	var decision RouteDecision
	if err := json.Unmarshal([]byte(resp.Response), &decision); err != nil {
		return nil, fmt.Errorf("failed to parse router JSON: %w\nRaw response: %s", err, resp.Response)
	}

	return &decision, nil
}

