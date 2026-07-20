package websocket

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	gorilla "github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/privacyruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/websocketauth"
)

// CommandResponse is emitted for command results that include an attachment.
type CommandResponse struct {
	Type        string                      `json:"type"`
	Response    string                      `json:"response,omitempty"`
	Attachment  *CommandResponseAttachment  `json:"attachment,omitempty"`
	Attachments []CommandResponseAttachment `json:"attachments,omitempty"`
}

// CommandResponseAttachment is a base64-encoded command attachment.
type CommandResponseAttachment struct {
	Filename string `json:"filename"`
	MIMEType string `json:"mime_type"`
	Data     string `json:"data"`
}

// Gateway handles local WebSocket connections for testing and client access.
type Gateway struct {
	Port          string
	Auth          *websocketauth.Store
	Links         *accountlinking.Service
	Runtime       gatewayruntime.Dependencies
	Log           *config.Logger
	connectionsMu sync.Mutex
	connections   map[string]map[*trackedConnection]struct{}
	clients       map[string]map[*trackedConnection]struct{}
}

type trackedConnection struct {
	conn    *gorilla.Conn
	writeMu sync.Mutex
}

func (c *trackedConnection) writeMessage(messageType int, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteMessage(messageType, data)
}

func (c *trackedConnection) closeWithReason(reason string) {
	c.writeMu.Lock()
	_ = c.conn.WriteControl(gorilla.CloseMessage, gorilla.FormatCloseMessage(gorilla.ClosePolicyViolation, reason), time.Now().Add(time.Second))
	_ = c.conn.Close()
	c.writeMu.Unlock()
}

// HandlePrivacyInvalidation closes authenticated connections only for deleted accounts.
func (wg *Gateway) HandlePrivacyInvalidation(event privacyruntime.Event) {
	if !event.CloseConnections {
		return
	}
	subjects := make(map[string]bool)
	clients := make(map[string]bool)
	const prefix = "websocket:"
	const clientPrefix = "websocket-client:"
	for _, external := range event.ExternalIdentities {
		if len(external) > len(clientPrefix) && external[:len(clientPrefix)] == clientPrefix {
			clients[external[len(clientPrefix):]] = true
		} else if len(external) > len(prefix) && external[:len(prefix)] == prefix {
			subjects[external[len(prefix):]] = true
		}
	}
	wg.connectionsMu.Lock()
	connectionSet := make(map[*trackedConnection]struct{})
	for subject := range subjects {
		for conn := range wg.connections[subject] {
			connectionSet[conn] = struct{}{}
		}
		delete(wg.connections, subject)
	}
	for clientID := range clients {
		for conn := range wg.clients[clientID] {
			connectionSet[conn] = struct{}{}
		}
		delete(wg.clients, clientID)
	}
	wg.connectionsMu.Unlock()
	for conn := range connectionSet {
		conn.closeWithReason("authorization revoked")
	}
}

func (wg *Gateway) log() *config.Logger {
	return wg.Log.Server("gateway.websocket", config.F("gateway", "websocket"))
}

// IncomingMessage is the JSON payload clients send over the WebSocket connection.
// UserID is retained for client compatibility and must match the authenticated
// token subject when present; it never selects request ownership.
// DisplayName is an optional human-readable name for the sender; it is injected
// into the system prompt so the model knows who it is speaking with.
// Prompt is the user's message text.
//
// Clients may send a plain text string instead; the raw text becomes the prompt
// while identity and session ownership still come from the handshake token.
type IncomingMessage struct {
	UserID      string          `json:"user_id"`
	DisplayName string          `json:"display_name"`
	Prompt      string          `json:"prompt"`
	Images      []IncomingImage `json:"images,omitempty"`
}

// IncomingImage is a base64-encoded image sent over the WebSocket connection.
type IncomingImage struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
	Source   string `json:"source,omitempty"`
}

var upgrader = gorilla.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     originAllowed,
}

func originAllowed(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	origin, err := url.Parse(origins[0])
	if err != nil || !origin.IsAbs() || (origin.Scheme != "http" && origin.Scheme != "https") || origin.User != nil || (origin.Path != "" && origin.Path != "/") || origin.RawQuery != "" || origin.Fragment != "" {
		return false
	}
	return strings.EqualFold(origin.Host, r.Host)
}
