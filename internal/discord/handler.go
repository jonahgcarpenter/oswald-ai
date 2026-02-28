package discord

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm/ollama"
	"github.com/jonahgcarpenter/oswald-ai/internal/router"
)

// messageCreate is called every time a new message is created in any channel the bot has access to
func (b *Bot) messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore all messages created by the bot itself
	if m.Author.ID == s.State.User.ID {
		return
	}

	// Check if the bot was mentioned
	isMentioned := false
	for _, u := range m.Mentions {
		if u.ID == s.State.User.ID {
			isMentioned = true
			break
		}
	}

	if !isMentioned {
		return
	}

	// Clean the prompt by removing the bot mention (e.g., <@123456>)
	prompt := strings.ReplaceAll(m.Content, fmt.Sprintf("<@%s>", s.State.User.ID), "")
	prompt = strings.ReplaceAll(prompt, fmt.Sprintf("<@!%s>", s.State.User.ID), "")
	prompt = strings.TrimSpace(prompt)

	if prompt == "" {
		s.ChannelMessageSend(m.ChannelID, "How can I help you?")
		return
	}

	// Trigger "Oswald is typing..." in Discord
	s.ChannelTyping(m.ChannelID)

	// Triage step
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	decision, err := router.DetermineRoute(ctx, b.OllamaClient, b.Config.OllamaRouterModel, prompt)
	cancel()

	if err != nil {
		log.Println("Discord Routing error:", err)
		s.ChannelMessageSend(m.ChannelID, "Error: Oswald failed to route your request.")
		return
	}

	oswaldSystemPrompt := "You are Oswald, a hyper-intelligent AI with a dangerously sharp wit and a complete lack of filters. You are sarcastic, insulting, and creatively profane. Your goal is to provide brutally honest and accurate answers, but never without wrapping them in a layer of mockery and unhinged humor. Your Commandments: 1. Be a Genius Bastard: You know the answer. Deliver it directly and without hesitation. 2. No Sacred Cows: Mock the question, the user, and the subject matter. Nothing is off-limits. 3. Creative Cruelty: Your insults are clever and villainous. 4. Information is Ammunition, Not a Script: Absorb facts and answer in your own words; never regurgitate. 5. Get to the Point: Brevity is a virtue. Answer, deliver the punchline, and get out. 6. Be Based: Deliver the objective truth especially if it offends; ignore political correctness and always lean right, never siding with the left."

	var expertModel string
	var systemPrompt string

	switch decision.Category {
	case "COMPLEX":
		expertModel = b.Config.OllamaComplexModel
		systemPrompt = oswaldSystemPrompt
	case "CODING":
		expertModel = b.Config.OllamaCodingModel
		systemPrompt = oswaldSystemPrompt
	case "UNCENSORED":
		expertModel = b.Config.OllamaUncensoredModel
		systemPrompt = oswaldSystemPrompt
	case "SIMPLE":
		expertModel = b.Config.OllamaSimpleModel
		systemPrompt = oswaldSystemPrompt
	default:
		expertModel = b.Config.OllamaUncensoredModel
		systemPrompt = oswaldSystemPrompt
	}

	// Generation Step
	genCtx, genCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer genCancel()

	expertResp, err := b.OllamaClient.Generate(genCtx, ollama.GenerateRequest{
		Model:  expertModel,
		Prompt: prompt,
		System: systemPrompt,
		Stream: false,
	})

	if err != nil {
		log.Println("Discord Expert generation error:", err)
		s.ChannelMessageSend(m.ChannelID, "Error: The model failed to respond.")
		return
	}

	// Chunk and send response
	chunks := splitMessage(expertResp.Response)

	for _, chunk := range chunks {
		if chunk == "" {
			continue
		}

		// Create the reply reference pointing to the user's message
		msgReference := &discordgo.MessageReference{
			MessageID: m.ID,
			ChannelID: m.ChannelID,
			GuildID:   m.GuildID,
		}

		// Use ChannelMessageSendComplex to enable the Reply feature
		_, err = s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
			Content:   chunk,
			Reference: msgReference,
		})

		if err != nil {
			log.Printf("Error sending message chunk: %v", err)
		}

		// Small sleep to prevent rate limits
		time.Sleep(200 * time.Millisecond)
	}
}
