package discord

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

const (
	gatewayURL = "wss://gateway.discord.gg/?v=10&encoding=json"
	apiBaseURL = "https://discord.com/api/v10"
	intents    = 37377 // GUILDS | GUILD_MESSAGES | MESSAGE_CONTENT | DIRECT_MESSAGES
)

// Payload is a raw Discord gateway event envelope.
type Payload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int            `json:"s,omitempty"`
	T  *string         `json:"t,omitempty"`
}

// HelloEvent is sent by Discord immediately after connect.
type HelloEvent struct {
	HeartbeatInterval time.Duration `json:"heartbeat_interval"`
}

// ReadyEvent carries the authenticated bot user identity.
type ReadyEvent struct {
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
}

// MessageCreate is the Discord MESSAGE_CREATE dispatch payload.
type MessageCreate struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id,omitempty"`
	Author    struct {
		ID       string `json:"id"`
		Bot      bool   `json:"bot"`
		Username string `json:"username"`
	} `json:"author"`
	Mentions []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"mentions,omitempty"`
	ReferencedMessage *struct {
		ID      string `json:"id"`
		Content string `json:"content"`
		Author  struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"author"`
	} `json:"referenced_message,omitempty"`
}

// createMessageResponse is the minimal Discord API response for a created message.
type createMessageResponse struct {
	ID string `json:"id"`
}

// replyContext records the routing metadata for a message Oswald sent, so that
// reply-chain lookups can determine which session and channel a prior message belongs to.
type replyContext struct {
	SessionKey string
	ChannelID  string
	SenderID   string
	CreatedAt  time.Time
}

// Gateway runs the Discord gateway connection loop.
type Gateway struct {
	Token      string
	BotID      string
	Broker     *broker.Broker
	Log        *config.Logger
	replyMu    sync.RWMutex
	replyIndex map[string]replyContext
}
