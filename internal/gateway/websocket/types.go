package websocket

import (
	"net/http"

	gorilla "github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// Gateway handles local WebSocket connections for testing and client access.
type Gateway struct {
	Port string
	Log  *config.Logger
}

var upgrader = gorilla.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}
