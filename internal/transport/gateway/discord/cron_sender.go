package discord

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/cron"
)

// cronChannelCache bounds remembered origin destinations.
const cronChannelCacheLimit = 1024

var (
	cronChannelMu    sync.Mutex
	cronChannelIndex = make(map[string]cronChannelEntry)
)

type cronChannelEntry struct {
	channelID string
	seen      time.Time
}

// rememberCronChannel records an inbound session's channel so origin
// delivery can route back without new discovery calls.
func rememberCronChannel(sessionKey, channelID string) {
	sessionKey, channelID = strings.TrimSpace(sessionKey), strings.TrimSpace(channelID)
	if sessionKey == "" || channelID == "" {
		return
	}
	cronChannelMu.Lock()
	defer cronChannelMu.Unlock()
	cronChannelIndex[sessionKey] = cronChannelEntry{channelID: channelID, seen: time.Now()}
	for len(cronChannelIndex) > cronChannelCacheLimit {
		oldest, at := "", time.Now()
		for key, entry := range cronChannelIndex {
			if entry.seen.Before(at) {
				oldest, at = key, entry.seen
			}
		}
		if oldest == "" {
			break
		}
		delete(cronChannelIndex, oldest)
	}
}

func lookupCronChannel(sessionKey string) (string, bool) {
	cronChannelMu.Lock()
	defer cronChannelMu.Unlock()
	entry, ok := cronChannelIndex[strings.TrimSpace(sessionKey)]
	return entry.channelID, ok && entry.channelID != ""
}

// CronSender returns a cron delivery sender for origin sessions.
func (dg *Gateway) CronSender() cron.Sender { return cronSender{gateway: dg} }

type cronSender struct {
	gateway *Gateway
}

// SendText delivers finished job output to an origin session: group
// sessions carry their channel in the key, DMs resolve through remembered
// inbound traffic. Text is chunked to Discord-sized messages.
func (s cronSender) SendText(ctx context.Context, _, sessionKey, text string) error {
	if s.gateway == nil {
		return errors.New("discord gateway is unavailable")
	}
	channel := ""
	parts := strings.SplitN(strings.TrimSpace(sessionKey), ":", 3)
	if len(parts) == 3 && parts[0] == "discord" && parts[1] != "dm" && parts[1] != "" {
		channel = parts[1]
	} else {
		var ok bool
		channel, ok = lookupCronChannel(sessionKey)
		if !ok {
			return errors.New("no live channel binding for origin session")
		}
	}
	for _, chunk := range chunkDiscordText(text) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := s.gateway.sendMessage(channel, chunk, ""); err != nil {
			return err
		}
	}
	return nil
}

func chunkDiscordText(text string) []string {
	const limit = 2000
	runes := []rune(strings.TrimSpace(text))
	if len(runes) == 0 {
		return []string{"(empty result)"}
	}
	var chunks []string
	for len(runes) > limit {
		cut := limit
		for i := limit - 1; i > limit-200 && i > 0; i-- {
			if runes[i] == '\n' {
				cut = i + 1
				break
			}
		}
		chunks = append(chunks, string(runes[:cut]))
		runes = runes[cut:]
	}
	return append(chunks, string(runes))
}
