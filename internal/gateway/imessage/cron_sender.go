package imessage

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/cron"
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

// SendText delivers finished job output to an origin session: group
// sessions carry their chat GUID in the key, DMs resolve through
// remembered inbound traffic.
func (s cronSender) SendText(ctx context.Context, _, sessionKey, text string) error {
	if s.gateway == nil {
		return errors.New("imessage gateway is unavailable")
	}
	guid := ""
	parts := strings.SplitN(strings.TrimSpace(sessionKey), ":", 3)
	if len(parts) == 3 && parts[0] == "imessage" && parts[1] != "dm" && parts[1] != "" {
		guid = parts[1]
	} else {
		var ok bool
		guid, ok = lookupCronChat(sessionKey)
		if !ok {
			return errors.New("no live chat binding for origin session")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		text = "(empty result)"
	}
	_, err := s.gateway.sendTextReply(guid, text, "", 0)
	return err
}
