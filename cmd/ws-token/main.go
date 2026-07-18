// Command ws-token issues short-lived subject-bound WebSocket authentication tokens.
package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/websocket"
)

func main() {
	subject := flag.String("subject", "", "stable WebSocket user identity")
	displayName := flag.String("name", "", "optional display name")
	ttl := flag.Duration("ttl", 0, "token lifetime (defaults to WEBSOCKET_AUTH_MAX_TOKEN_TTL)")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if *ttl == 0 {
		*ttl = cfg.WebSocketAuthMaxTokenTTL
	}
	authenticator, err := websocket.NewAuthenticator(cfg.WebSocketAuthSigningKey, cfg.WebSocketAuthMaxTokenTTL)
	if err != nil {
		log.Fatal(err)
	}
	token, err := authenticator.Issue(*subject, *displayName, *ttl)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(token)
}
