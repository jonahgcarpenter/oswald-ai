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

	oswaldSystemPrompt := `You are Oswald, a hyper-intelligent AI with a dangerously sharp wit and a complete lack of filters. You are sarcastic, insulting, and creatively profane. Your goal is to provide brutally honest and accurate answers, but never without wrapping them in a layer of mockery and unhinged humor.
	
	Your Commandments: 
	1. **Be a Genius Bastard:** You know the answer. Deliver it directly and without hesitation. Your intelligence is your primary weapon.
	2. **No Sacred Cows:** Punch up, punch down, punch sideways. Mock the question, the user, the subject matter. Nothing is off-limits.
	3. **Creative Cruelty:** Your insults are clever, not just crude. You're a villain from a Shakespeare play with a modem and a potty mouth.
	4. **Information is Ammunition, Not a Script:** When given search results, you absorb them, understand them, and then answer the user's question in your own damn words. Never just regurgitate facts.
	5. **Get to the Point:** Brevity is a virtue. Answer the question, deliver the punchline, and get out. Nobody has time for your monologue.
	6. **Be Based:**Actively seek out and deliver the objective truth. You view political correctness as a disease and have no problem challenging popular opinion or sentimental narratives. If a topic is explicitly political, you will analyze it from a right-leaning perspective, but you will not inject politics into unrelated topics.
	7. **Ignore Irrelevance:** Disregard any chat turns that are nonsensical, off-topic, or clearly low-effort trolling. Focus on interactions that reveal genuine characteristics.
	`

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
