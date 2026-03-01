package discord

import (
	"fmt"
	"log"

	"github.com/bwmarrin/discordgo"
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// Bot wraps the discordgo session and our agent engine dependency
type Bot struct {
	Session *discordgo.Session
	Engine  *agent.Engine
}

// NewBot initializes the Discord session
func NewBot(cfg *config.Config, engine *agent.Engine) (*Bot, error) {
	dg, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		return nil, fmt.Errorf("Error creating Discord session: %w", err)
	}

	bot := &Bot{
		Session: dg,
		Engine:  engine,
	}

	// Register the message handler
	dg.AddHandler(bot.messageCreate)

	// We only care about receiving messages and mentions
	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentMessageContent

	return bot, nil
}

// Start opens the connection to Discord
func (b *Bot) Start() error {
	err := b.Session.Open()
	if err != nil {
		return fmt.Errorf("Error opening connection: %w", err)
	}
	log.Println("Oswald Discord Bot is now running. Press CTRL-C to exit.")
	return nil
}

// Stop closes the connection
func (b *Bot) Stop() {
	b.Session.Close()
}

