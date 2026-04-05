package websocket

import (
	"net/http"

	gorilla "github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/accountlink"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// Gateway handles local WebSocket connections for testing and client access.
type Gateway struct {
	Port     string
	Links    *accountlink.Service
	Commands *accountlink.CommandHandler
	Log      *config.Logger
}

// IncomingMessage is the JSON payload clients send over the WebSocket connection.
// UserID identifies the sender for persistent memory and session keying.
// DisplayName is an optional human-readable name for the sender; it is injected
// into the system prompt so the model knows who it is speaking with.
// Prompt is the user's message text.
//
// Clients that send a plain text string (non-JSON) are handled with legacy
// fallback behaviour: the raw text is used as the prompt and the connection's
// remote address is used as both the user ID and session key.
type IncomingMessage struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
	Prompt      string `json:"prompt"`
}

var upgrader = gorilla.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}
