package imessage

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/cron"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
)

// cronChatCache bounds remembered origin destinations.
const cronChatCacheLimit = 1024

var (
	cronChatMu    sync.Mutex
	cronChatIndex = make(map[string]cronChatEntry)
)

type cronChatEntry struct {
	chatGUID string
	seen     time.Time
}

// rememberCronChat records an inbound session's chat GUID so origin
// delivery can route back without new discovery calls.
func rememberCronChat(sessionKey, chatGUID string) {
	sessionKey, chatGUID = strings.TrimSpace(sessionKey), strings.TrimSpace(chatGUID)
	if sessionKey == "" || chatGUID == "" {
		return
	}
	cronChatMu.Lock()
	defer cronChatMu.Unlock()
	cronChatIndex[sessionKey] = cronChatEntry{chatGUID: chatGUID, seen: time.Now()}
	for len(cronChatIndex) > cronChatCacheLimit {
		oldest, at := "", time.Now()
		for key, entry := range cronChatIndex {
			if entry.seen.Before(at) {
				oldest, at = key, entry.seen
			}
		}
		if oldest == "" {
			break
		}
		delete(cronChatIndex, oldest)
	}
}

func lookupCronChat(sessionKey string) (string, bool) {
	cronChatMu.Lock()
	defer cronChatMu.Unlock()
	entry, ok := cronChatIndex[strings.TrimSpace(sessionKey)]
	return entry.chatGUID, ok && entry.chatGUID != ""
}

// CronSender returns a cron delivery sender for origin sessions.
func (g *Gateway) CronSender() cron.Sender { return cronSender{gateway: g} }

type cronSender struct {
	gateway *Gateway
}

// Deliver delivers finished job output to an origin session. Group
// sessions carry their chat GUID in the key; DM sessions resolve through
// remembered inbound traffic, falling back to the canonical BlueBubbles
// iMessage direct-chat GUID ("iMessage;-;<handle>") so delivery works even
// with no inbound traffic since process start. The service segment must be
// the concrete "iMessage" service: the "any" service is only meaningful for
// reading, and BlueBubbles send endpoints reject it (the AppleScript
// fallback fails, and non-AppleScript methods report "Chat does not exist").
// Text goes first and attachments follow top-level in the same chat: cron
// turns answer no user message, so there is nothing to thread to.
func (s cronSender) Deliver(ctx context.Context, _, sessionKey, text string, attachments []media.OutputAttachment) error {
	if s.gateway == nil {
		return errors.New("imessage gateway is unavailable")
	}
	key := strings.TrimSpace(sessionKey)
	parts := strings.SplitN(key, ":", 3)
	var dmPeer string
	guid := ""
	if len(parts) == 3 && parts[0] == "imessage" && parts[1] != "" {
		if parts[1] == "dm" {
			dmPeer = strings.TrimSpace(parts[2])
		} else {
			guid = parts[1]
		}
	}
	if dmPeer != "" {
		if remembered, ok := lookupCronChat(key); ok {
			guid = remembered
		} else {
			guid = "iMessage;-;" + dmPeer
		}
	} else if guid == "" {
		var ok bool
		guid, ok = lookupCronChat(key)
		if !ok {
			return errors.New("no live chat binding for origin session " + key)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" && len(attachments) == 0 {
		text = "(empty result)"
	}
	if text != "" {
		if _, err := s.gateway.sendTextReply(guid, text, "", 0); err != nil {
			return err
		}
	}
	for _, attachment := range attachments {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.gateway.sendCommandAttachment(guid, "", attachment); err != nil {
			return err
		}
	}
	return nil
}
