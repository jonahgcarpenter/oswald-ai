package discord

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
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

	// Delegate ALL logic to the agent engine
	agentResp, err := b.Engine.Process(prompt)
	if err != nil {
		log.Println("Discord Processing error:", err)
		s.ChannelMessageSend(m.ChannelID, "Error: Oswald failed to process your request.")
		return
	}

	if agentResp.Error != "" {
		s.ChannelMessageSend(m.ChannelID, agentResp.Error)
		return
	}

	// Chunk and send response
	chunks := splitMessage(agentResp.Response)

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
