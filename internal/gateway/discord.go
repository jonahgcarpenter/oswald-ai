package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
)

const (
	gatewayURL = "wss://gateway.discord.gg/?v=10&encoding=json"
	apiBaseURL = "https://discord.com/api/v10"
	intents    = 37377 // GUILDS | GUILD_MESSAGES | MESSAGE_CONTENT | DIRECT_MESSAGES
)

// Gateway structures
type Payload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int            `json:"s,omitempty"`
	T  *string         `json:"t,omitempty"`
}

type HelloEvent struct {
	HeartbeatInterval time.Duration `json:"heartbeat_interval"`
}

type ReadyEvent struct {
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
}

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
}

type DiscordGateway struct {
	Token string
	BotID string
	Agent *agent.Agent
}

func (dg *DiscordGateway) Name() string {
	return "Discord"
}

var botToken string
var botUserID string

// Start initializes the resilient connection loop.
// It will block forever, automatically reconnecting if the websocket drops.
func (dg *DiscordGateway) Start(aiAgent *agent.Agent) error {
	dg.Agent = aiAgent

	for {
		err := dg.connectAndListen()

		if err != nil {
			log.Printf("Discord connection dropped: %v", err)
		} else {
			log.Println("Discord connection closed normally.")
		}

		// Wait 5 seconds before trying to reconnect to avoid spamming Discord's API
		log.Println("Reconnecting to Discord Gateway in 5 seconds...")
		time.Sleep(5 * time.Second)
	}
}

// Handles a single, continuous websocket session
func (dg *DiscordGateway) connectAndListen() error {
	conn, _, err := websocket.DefaultDialer.Dial(gatewayURL, nil)
	if err != nil {
		return fmt.Errorf("Failed to dial Discord Gateway: %w", err)
	}

	// This ensures the connection is closed when this function exits.
	defer conn.Close()

	// Wait for HELLO
	var helloPayload Payload
	if err := conn.ReadJSON(&helloPayload); err != nil || helloPayload.Op != 10 {
		return fmt.Errorf("Expected HELLO payload: %v", err)
	}

	var hello HelloEvent
	json.Unmarshal(helloPayload.D, &hello)

	// Start Heartbeat
	go dg.heartbeatLoop(conn, hello.HeartbeatInterval*time.Millisecond)

	// Identify
	if err := dg.identify(conn); err != nil {
		return fmt.Errorf("Failed to identify: %w", err)
	}

	// Block and listen for messages.
	return dg.listenLoop(conn)
}

func (dg *DiscordGateway) identify(conn *websocket.Conn) error {
	identifyData := map[string]interface{}{
		"token":   dg.Token,
		"intents": intents,
		"properties": map[string]string{
			"os":      "linux",
			"browser": "oswald-ai",
			"device":  "oswald-ai",
		},
	}

	identifyPayload := Payload{
		Op: 2,
		D:  marshalJSON(identifyData),
	}

	if err := conn.WriteJSON(identifyPayload); err != nil {
		return fmt.Errorf("Failed to send IDENTIFY: %w", err)
	}
	return nil
}

func (dg *DiscordGateway) heartbeatLoop(conn *websocket.Conn, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		hb := Payload{Op: 1, D: []byte("null")}
		if err := conn.WriteJSON(hb); err != nil {
			log.Printf("Heartbeat failed: %v", err)
			return
		}
	}
}

func (dg *DiscordGateway) listenLoop(conn *websocket.Conn) error {
	for {
		var p Payload
		if err := conn.ReadJSON(&p); err != nil {
			return fmt.Errorf("Discord read error: %w", err)
		}

		switch p.Op {
		case 0:
			if p.T != nil {
				switch *p.T {
				case "READY":
					var ready ReadyEvent
					if err := json.Unmarshal(p.D, &ready); err == nil {
						dg.BotID = ready.User.ID
						displayName := ready.User.Username
						log.Printf("Discord Bot connected as: %s (ID: %s)", displayName, dg.BotID)
					}
				case "RESUMED":
					// Useful to know when the bot recovered without a full restart
					log.Println("Discord session resumed successfully.")
				case "MESSAGE_CREATE":
					var msg MessageCreate
					if err := json.Unmarshal(p.D, &msg); err == nil {
						dg.handleMessage(msg)
					}
				}
			}
		case 7: // Reconnect
			return fmt.Errorf("Discord requested a reconnect")
		case 9: // Invalid Session
			// Discord rejected our session. Returning an error forces the bot
			// to drop the connection and start completely fresh in the Start() loop.
			return fmt.Errorf("Discord session invalid, forcing reconnect")
		case 11: // Heartbeat ACK
			// Discord received our heartbeat. We do nothing, but catching it
		}
	}
}

// splitMessage breaks a large string into chunks, prioritizing newlines, sentence ends, and spaces.
func splitMessage(text string, limit int) []string {
	var chunks []string
	runes := []rune(text)

	for len(runes) > 0 {
		// If the remaining text fits, add it and finish
		if len(runes) <= limit {
			chunks = append(chunks, string(runes))
			break
		}

		// Create a window of the maximum allowed size
		chunkRunes := runes[:limit]
		splitIdx := -1

		// Priority 1: Split at the last newline
		for i := len(chunkRunes) - 1; i >= 0; i-- {
			if chunkRunes[i] == '\n' {
				splitIdx = i
				break
			}
		}

		// Priority 2: If no newline, split at the last sentence boundary (. ! ?)
		if splitIdx == -1 {
			for i := len(chunkRunes) - 1; i > 0; i-- {
				if (chunkRunes[i-1] == '.' || chunkRunes[i-1] == '!' || chunkRunes[i-1] == '?') && chunkRunes[i] == ' ' {
					splitIdx = i // Split at the space
					break
				}
			}
		}

		// Priority 3: If no sentence boundary, split at the last space
		if splitIdx == -1 {
			for i := len(chunkRunes) - 1; i >= 0; i-- {
				if chunkRunes[i] == ' ' {
					splitIdx = i
					break
				}
			}
		}

		// Priority 4: Fallback, hard split at the limit if it's one massive unbroken string
		if splitIdx == -1 {
			splitIdx = limit
		}

		// Save the chunk, trimming any trailing whitespace
		chunks = append(chunks, strings.TrimSpace(string(runes[:splitIdx])))

		// Advance the slice for the next iteration
		runes = runes[splitIdx:]
		// Clean up leading spaces/newlines so the next chunk doesn't start weirdly
		runes = []rune(strings.TrimLeft(string(runes), " \n\r"))
	}

	return chunks
}

func (dg *DiscordGateway) handleMessage(msg MessageCreate) {
	// Skip messages from bots
	if msg.Author.Bot {
		return
	}

	replyToID := ""

	prompt := msg.Content

	if msg.GuildID != "" {
		mention1 := fmt.Sprintf("<@%s>", dg.BotID)
		mention2 := fmt.Sprintf("<@!%s>", dg.BotID)

		if !strings.Contains(msg.Content, mention1) && !strings.Contains(msg.Content, mention2) {
			return
		}
		replyToID = msg.ID

		// Strip the mention out of the text so the AI doesn't see "<@12345> hello"
		prompt = strings.ReplaceAll(prompt, mention1, "")
		prompt = strings.ReplaceAll(prompt, mention2, "")
		prompt = strings.TrimSpace(prompt)
	}

	// Strip empty messages
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		dg.sendMessage(msg.ChannelID, "What do you want idiot.", replyToID)
		return
	}

	// Turn custom Discord emojis into readable formats for the LLM
	// Changes <:smile:123456789> into :smile:
	re := regexp.MustCompile(`<a?:([^:]+):\d+>`)
	prompt = re.ReplaceAllString(prompt, ":$1:")

	// FIX: Remove these logs eventually
	displayName := msg.Author.Username
	log.Printf("Discord Request from %s (ID: %s): %s", displayName, msg.Author.ID, prompt)

	// Send typing indicator in discord
	stopTyping := make(chan struct{})

	// Ensures this channel closes the exact moment handleMessage finishes
	defer close(stopTyping)

	go func() {
		// Trigger immediately
		_ = dg.sendTyping(msg.ChannelID)

		// Set a ticker for 9 seconds to refresh the 10-second indicator
		ticker := time.NewTicker(9 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				_ = dg.sendTyping(msg.ChannelID)
			case <-stopTyping:
				return // Kill this background loop when signaled
			}
		}
	}()

	// Pass the text to the agent, nil to disable streaming
	finalPayload, err := dg.Agent.Process(prompt, nil)

	if err != nil {
		log.Printf("Agent process error: %v", err)
		dg.sendMessage(msg.ChannelID, "Sorry, I encountered an internal error processing that.", replyToID)
		return
	}

	responseText := finalPayload.Response

	// Discord's 2,000 char limit
	chunks := splitMessage(responseText, 2000)

	// Send each chunk
	for i, chunk := range chunks {
		// Only attach the reply reference to the very first chunk
		currentReplyID := ""
		if i == 0 {
			currentReplyID = replyToID
		}

		err = dg.sendMessage(msg.ChannelID, chunk, currentReplyID)
		if err != nil {
			log.Printf("Failed to send chunk %d to Discord: %v", i+1, err)
			break // Stop trying to send the rest if a chunk fails
		}
	}
}

func (dg *DiscordGateway) sendTyping(channelID string) error {
	url := fmt.Sprintf("%s/channels/%s/typing", apiBaseURL, channelID)

	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bot "+dg.Token)

	// A short timeout is fine here, it's a lightweight request
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	return nil
}

func (dg *DiscordGateway) sendMessage(channelID, content, replyToID string) error {
	url := fmt.Sprintf("%s/channels/%s/messages", apiBaseURL, channelID)

	// Use a map[string]interface{} so we can conditionally add nested objects
	payload := map[string]interface{}{
		"content": content,
	}

	// If a replyToID was provided, attach the message_reference object
	if replyToID != "" {
		payload["message_reference"] = map[string]string{
			"message_id": replyToID,
		}
	}

	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bot "+dg.Token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	return nil
}

func marshalJSON(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
