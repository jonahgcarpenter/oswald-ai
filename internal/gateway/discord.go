package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
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
	// ReferencedMessage is populated by Discord when this message is a reply.
	ReferencedMessage *struct {
		Content string `json:"content"`
		Author  struct {
			Username string `json:"username"`
		} `json:"author"`
	} `json:"referenced_message,omitempty"`
}

type DiscordGateway struct {
	Token string
	BotID string
	Agent *agent.Agent
	Log   *config.Logger
}

func (dg *DiscordGateway) Name() string {
	return "Discord"
}

// Start initializes the resilient connection loop.
// It will block forever, automatically reconnecting if the websocket drops.
func (dg *DiscordGateway) Start(aiAgent *agent.Agent) error {
	dg.Agent = aiAgent

	for {
		err := dg.connectAndListen()

		if err != nil {
			dg.Log.Warn("Discord connection dropped: %v", err)
		} else {
			dg.Log.Info("Discord connection closed normally.")
		}

		// TODO: Implement exponential backoff with max delay.
		// Currently retries every 5 seconds, which may overwhelm Discord's API on sustained outages.
		dg.Log.Info("Reconnecting to Discord Gateway in 5 seconds...")
		time.Sleep(5 * time.Second)
	}
}

// connectAndListen manages a single Discord gateway session, handling authentication,
// heartbeat loops, and message routing. Returns when the connection drops or Discord
// requests a reconnection.
func (dg *DiscordGateway) connectAndListen() error {
	conn, _, err := websocket.DefaultDialer.Dial(gatewayURL, nil)
	if err != nil {
		return fmt.Errorf("Failed to dial Discord Gateway: %w", err)
	}

	// This ensures the connection is closed when this function exits.
	defer conn.Close()

	// Wait for HELLO to receive heartbeat interval
	var helloPayload Payload
	if err := conn.ReadJSON(&helloPayload); err != nil || helloPayload.Op != 10 {
		return fmt.Errorf("Expected HELLO payload: %v", err)
	}

	var hello HelloEvent
	json.Unmarshal(helloPayload.D, &hello)

	// Start heartbeat loop with Discord's specified interval
	go dg.heartbeatLoop(conn, hello.HeartbeatInterval*time.Millisecond)

	// Identify to Discord with bot token and intents
	if err := dg.identify(conn); err != nil {
		return fmt.Errorf("Failed to identify: %w", err)
	}

	// Block and listen for incoming messages until connection closes
	return dg.listenLoop(conn)
}

// identify sends the IDENTIFY opcode to Discord, authenticating the bot with its token and intents.
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

// heartbeatLoop sends heartbeat packets to Discord at the specified interval.
// Exits when the connection closes or a send fails. Discord requires heartbeats
// to maintain the session; missing them triggers automatic disconnection.
func (dg *DiscordGateway) heartbeatLoop(conn *websocket.Conn, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		hb := Payload{Op: 1, D: []byte("null")}
		if err := conn.WriteJSON(hb); err != nil {
			dg.Log.Error("Heartbeat failed: %v", err)
			return
		}
	}
}

// listenLoop continuously reads events from the Discord gateway and dispatches them
// to handlers. Returns when the connection closes or Discord sends a reconnect/invalid
// session opcode.
func (dg *DiscordGateway) listenLoop(conn *websocket.Conn) error {
	for {
		var p Payload
		if err := conn.ReadJSON(&p); err != nil {
			return fmt.Errorf("Discord read error: %w", err)
		}

		switch p.Op {
		case 0: // Dispatch event
			if p.T != nil {
				switch *p.T {
				case "READY":
					// Session established; capture bot identity
					var ready ReadyEvent
					if err := json.Unmarshal(p.D, &ready); err == nil {
						dg.BotID = ready.User.ID
						dg.Log.Info("Discord Bot connected as: %s (ID: %s)", ready.User.Username, dg.BotID)
					}
				case "RESUMED":
					// Session resumed after a drop; bot is already connected
					dg.Log.Info("Discord session resumed successfully.")
				case "MESSAGE_CREATE":
					// Incoming message; route to handler for processing
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
			// Discord acknowledged our heartbeat; connection is healthy
		}
	}
}

// splitMessage breaks a large string into chunks respecting Discord's 2000-char limit.
// Prioritizes splitting at newlines (P1), sentence boundaries (P2), word spaces (P3),
// and falls back to hard split if the text is one massive unbroken string (P4).
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

// handleMessage processes an incoming Discord message, validating it, extracting context,
// sending it to the agent, and streaming responses back to Discord.
func (dg *DiscordGateway) handleMessage(msg MessageCreate) {
	// Skip messages from bots (avoid feedback loops)
	if msg.Author.Bot {
		return
	}

	replyToID := ""
	prompt := msg.Content

	// In guild channels, require an explicit bot mention; in DMs, process all messages
	if msg.GuildID != "" {
		mention1 := fmt.Sprintf("<@%s>", dg.BotID)
		mention2 := fmt.Sprintf("<@!%s>", dg.BotID)

		if !strings.Contains(msg.Content, mention1) && !strings.Contains(msg.Content, mention2) {
			return
		}
		replyToID = msg.ID

		// Strip the mention from the prompt so the LLM doesn't see "<@12345> hello"
		prompt = strings.ReplaceAll(prompt, mention1, "")
		prompt = strings.ReplaceAll(prompt, mention2, "")
		prompt = strings.TrimSpace(prompt)
	}

	// Validate prompt isn't empty after mention stripping
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		dg.sendMessage(msg.ChannelID, "What do you want idiot.", replyToID)
		return
	}

	// Normalize custom Discord emoji format for the LLM
	// Changes <:smile:123456789> into :smile: for readability
	re := regexp.MustCompile(`<a?:([^:]+):\d+>`)
	prompt = re.ReplaceAllString(prompt, ":$1:")

	// Include quoted context if this is a reply to another message
	if msg.ReferencedMessage != nil && msg.ReferencedMessage.Content != "" {
		quotedContent := re.ReplaceAllString(msg.ReferencedMessage.Content, ":$1:")
		prompt = fmt.Sprintf("[Replying to %s: \"%s\"]\n%s",
			msg.ReferencedMessage.Author.Username,
			quotedContent,
			prompt,
		)
	}

	dg.Log.Info("Discord request from %s: %q", msg.Author.Username, truncate(prompt, 100))

	// Launch typing indicator loop; signals when message handling completes
	stopTyping := make(chan struct{})
	defer close(stopTyping)

	go func() {
		// Trigger immediately to show the bot is processing
		_ = dg.sendTyping(msg.ChannelID)

		// Discord typing indicator lasts 10 seconds; refresh every 9 to stay visible
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

	// Send prompt to agent for routing and response generation
	finalPayload, err := dg.Agent.Process(prompt, nil)

	if err != nil {
		dg.Log.Error("Agent process error: %v", err)
		dg.sendMessage(msg.ChannelID, "Sorry, I encountered an internal error processing that.", replyToID)
		return
	}

	responseText := finalPayload.Response

	// NOTE: Discord enforces a 2000-character limit per message and silently truncates.
	// We detect and split preemptively to avoid losing content.
	chunks := splitMessage(responseText, 2000)

	dg.Log.Debug("Discord response to %s: %d chunk(s), %d chars, model: %s",
		msg.Author.Username, len(chunks), len(responseText), finalPayload.Model)

	// Send each chunk, attaching the reply reference only to the first chunk
	for i, chunk := range chunks {
		currentReplyID := ""
		if i == 0 {
			currentReplyID = replyToID
		}

		err = dg.sendMessage(msg.ChannelID, chunk, currentReplyID)
		if err != nil {
			dg.Log.Error("Failed to send chunk %d to Discord: %v", i+1, err)
			break // Stop trying to send the rest if a chunk fails
		}
	}
}

// sendTyping posts a typing indicator to Discord, showing that the bot is processing a message.
// Discord's typing indicator lasts 10 seconds, so callers should refresh periodically.
func (dg *DiscordGateway) sendTyping(channelID string) error {
	url := fmt.Sprintf("%s/channels/%s/typing", apiBaseURL, channelID)

	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bot "+dg.Token)

	// Lightweight request; short timeout is acceptable
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

// sendMessage posts a message to a Discord channel, optionally as a reply to another message.
func (dg *DiscordGateway) sendMessage(channelID, content, replyToID string) error {
	url := fmt.Sprintf("%s/channels/%s/messages", apiBaseURL, channelID)

	// Build payload with optional message_reference for replies
	payload := map[string]interface{}{
		"content": content,
	}

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

// marshalJSON converts any value to a json.RawMessage, ignoring marshal errors.
// Used for Discord gateway payloads where the structure is known to be JSON-safe.
func marshalJSON(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// truncate returns s shortened to at most max runes, appending "…" if cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
