package ws

import (
	"fmt"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
)

// Upgrades HTTP connection to WebSocket
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func HandleConnections(w http.ResponseWriter, r *http.Request) {
	// Upgrade init request to Websocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrader error:", err)
		return
	}
	// Close if err
	defer conn.Close()

	fmt.Println("New client connected")

	// Loop for read and write
	for {
		// Read
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Println("Read error", err)
			break
		}

		// Print message
		fmt.Printf("Received: %s\n", message)

		// Return message to client
		err = conn.WriteMessage(messageType, message)
		if err != nil {
			log.Println("Write error:", err)
			break
		}
	}
}
