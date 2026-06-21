package discord

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
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
	SessionID        string `json:"session_id"`
	ResumeGatewayURL string `json:"resume_gateway_url"`
	User             struct {
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
	Attachments       []Attachment `json:"attachments,omitempty"`
	Embeds            []Embed      `json:"embeds,omitempty"`
	ReferencedMessage *struct {
		ID          string       `json:"id"`
		Content     string       `json:"content"`
		Attachments []Attachment `json:"attachments,omitempty"`
		Embeds      []Embed      `json:"embeds,omitempty"`
		Author      struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"author"`
	} `json:"referenced_message,omitempty"`
}

// messageResponse is the minimal Discord API response for a fetched message.
type messageResponse struct {
	ID          string       `json:"id"`
	Content     string       `json:"content"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Embeds      []Embed      `json:"embeds,omitempty"`
	Author      struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"author"`
}

// Embed describes Discord link-preview media that can be treated like an image.
type Embed struct {
	Type      string     `json:"type,omitempty"`
	URL       string     `json:"url,omitempty"`
	Image     EmbedImage `json:"image,omitempty"`
	Thumbnail EmbedImage `json:"thumbnail,omitempty"`
}

// EmbedImage describes an image-like Discord embed asset.
type EmbedImage struct {
	URL      string `json:"url,omitempty"`
	ProxyURL string `json:"proxy_url,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

// Attachment describes a Discord message attachment relevant to gateway routing.
type Attachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	Size        int    `json:"size,omitempty"`
	URL         string `json:"url,omitempty"`
	ProxyURL    string `json:"proxy_url,omitempty"`
}

// createMessageResponse is the minimal Discord API response for a created message.
type createMessageResponse struct {
	ID string `json:"id"`
}

// replyContext records the routing metadata for a message Oswald sent, so that
// reply-chain lookups can determine which session and channel a prior message belongs to.
type replyContext struct {
	SessionKey  string
	ChannelID   string
	SenderID    string
	DisplayName string
	Text        string
	Attachments []Attachment
	Embeds      []Embed
	IsFromBot   bool
	CreatedAt   time.Time
}

// Gateway runs the Discord gateway connection loop.
type Gateway struct {
	Token      string
	BotID      string
	Broker     *broker.Broker
	Links      *accountlinking.Service
	Commands   *commands.Router
	Log        *config.Logger
	APIBaseURL string
	HTTPClient *http.Client
	replyMu    sync.RWMutex
	replyIndex map[string]replyContext
	sessionMu  sync.RWMutex
	sessionID  string
	resumeURL  string
	lastSeq    *int
	hbAcked    bool
}

func (dg *Gateway) apiBaseURL() string {
	if dg.APIBaseURL != "" {
		return dg.APIBaseURL
	}
	return apiBaseURL
}

func (dg *Gateway) httpClient(timeout time.Duration) *http.Client {
	if dg.HTTPClient != nil {
		return dg.HTTPClient
	}
	return &http.Client{Timeout: timeout}
}
