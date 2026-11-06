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

// resolveChatGUID maps an origin session key to its BlueBubbles chat GUID.
// Group sessions carry their GUID in the key; DMs resolve through remembered
// inbound traffic, falling back to the canonical direct-chat GUID so delivery
// works even with no inbound traffic since process start.
func (s cronSender) resolveChatGUID(sessionKey string) (string, error) {
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
			return remembered, nil
		}
		return "iMessage;-;" + dmPeer, nil
	}
	if guid != "" {
		return guid, nil
	}
	if remembered, ok := lookupCronChat(key); ok {
		return remembered, nil
	}
	return "", errors.New("no live chat binding for origin session " + key)
}

// DeliverMention sends a tappable mention bubble (single multipart part, so
// it stays one message) and threads the body under it as a reply. If the
// mention send fails it falls back to a plain send; if the threaded reply
// fails the body follows up as plain text so content is never lost.
func (s cronSender) DeliverMention(ctx context.Context, profile, sessionKey string, mention cron.Mention, text string, attachments []media.OutputAttachment) error {
	if s.gateway == nil {
		return errors.New("imessage gateway is unavailable")
	}
	guid, err := s.resolveChatGUID(sessionKey)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	header := strings.TrimSpace(profile + " - " + mention.Header)
	if strings.TrimSpace(profile) == "" {
		header = strings.TrimSpace(mention.Header)
	}
	address := strings.TrimSpace(mention.Address)
	mentionGUID, err := s.gateway.sendMultipart(guid, []multipartPart{{PartIndex: 0, Text: header, Mention: address}})
	if err != nil {
		return s.Deliver(ctx, "", sessionKey, text, attachments)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(text) != "" {
		if _, err := s.gateway.sendTextReply(guid, text, mentionGUID, 0); err != nil {
			return s.Deliver(ctx, "", sessionKey, text, attachments)
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
// with no inbound traffic since process start. GUIDs pass through verbatim:
// the server normalizes the service prefix (any/iMessage/SMS) to the stored
// chat on send, so Tahoe-style "any" GUIDs route without rewriting here.
// Text goes first and attachments follow top-level in the same chat: cron
// turns answer no user message, so there is nothing to thread to.
func (s cronSender) Deliver(ctx context.Context, _, sessionKey, text string, attachments []media.OutputAttachment) error {
	if s.gateway == nil {
		return errors.New("imessage gateway is unavailable")
	}
	guid, err := s.resolveChatGUID(sessionKey)
	if err != nil {
		return err
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
