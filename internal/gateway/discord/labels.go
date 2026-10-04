package discord

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

const (
	chatLabelMaxRunes = 100
	chatNameCacheTTL  = time.Hour
)

type labelNameCacheEntry struct {
	Name      string
	ExpiresAt time.Time
}

// chatLabel builds the untrusted conversation label shown in the system prompt.
// Dynamic names are sanitized; the surrounding structure is literal.
func (dg *Gateway) chatLabel(msg MessageCreate, log *config.Logger) string {
	user := sanitizeLabelValue(msg.Author.Username, chatLabelMaxRunes)
	if msg.GuildID == "" {
		if user == "" {
			return `"DM"`
		}
		return `"DM with ` + user + `"`
	}
	channel := dg.channelName(msg.ChannelID, log)
	guild := dg.guildName(msg.GuildID, log)
	switch {
	case channel != "" && guild != "":
		return `group channel "` + channel + `" on server "` + guild + `"`
	case channel != "":
		return `group channel "` + channel + `"`
	case guild != "":
		return `group on server "` + guild + `"`
	default:
		return `"group channel"`
	}
}

func (dg *Gateway) channelName(channelID string, log *config.Logger) string {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return ""
	}
	key := "channel:" + channelID
	if name, ok := dg.cachedLabelName(key); ok {
		return name
	}
	name := dg.fetchChannelName(channelID, log)
	if name != "" {
		dg.cacheLabelName(key, name)
	}
	return name
}

func (dg *Gateway) guildName(guildID string, log *config.Logger) string {
	guildID = strings.TrimSpace(guildID)
	if guildID == "" {
		return ""
	}
	key := "guild:" + guildID
	if name, ok := dg.cachedLabelName(key); ok {
		return name
	}
	name := dg.fetchGuildName(guildID, log)
	if name != "" {
		dg.cacheLabelName(key, name)
	}
	return name
}

func (dg *Gateway) fetchChannelName(channelID string, log *config.Logger) string {
	var result struct {
		Name string `json:"name"`
	}
	if !dg.getChatLabelJSON(fmt.Sprintf("%s/channels/%s", dg.apiBaseURL(), channelID), log, &result) {
		return ""
	}
	return sanitizeLabelValue(result.Name, chatLabelMaxRunes)
}

func (dg *Gateway) fetchGuildName(guildID string, log *config.Logger) string {
	var result struct {
		Name string `json:"name"`
	}
	if !dg.getChatLabelJSON(fmt.Sprintf("%s/guilds/%s", dg.apiBaseURL(), guildID), log, &result) {
		return ""
	}
	return sanitizeLabelValue(result.Name, chatLabelMaxRunes)
}

func (dg *Gateway) getChatLabelJSON(url string, log *config.Logger, out interface{}) bool {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		log.Debug("gateway.chat_label.failed", "failed to build discord chat label request", config.F("status", "degraded"), config.ErrorField(err))
		return false
	}
	req.Header.Set("Authorization", "Bot "+dg.Token)
	resp, err := dg.httpClient(10 * time.Second).Do(req)
	if err != nil {
		log.Debug("gateway.chat_label.failed", "discord chat label request failed", config.F("status", "degraded"), config.ErrorField(err))
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Debug("gateway.chat_label.failed", "discord chat label request failed", config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
		return false
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(out); err != nil {
		log.Debug("gateway.chat_label.failed", "failed to decode discord chat label response", config.F("status", "degraded"), config.ErrorField(err))
		return false
	}
	return true
}

func (dg *Gateway) cachedLabelName(key string) (string, bool) {
	dg.labelMu.Lock()
	defer dg.labelMu.Unlock()
	dg.pruneLabelNamesLocked()
	entry, ok := dg.labelNames[key]
	if !ok {
		return "", false
	}
	return entry.Name, true
}

func (dg *Gateway) cacheLabelName(key, name string) {
	dg.labelMu.Lock()
	defer dg.labelMu.Unlock()
	if dg.labelNames == nil {
		dg.labelNames = make(map[string]labelNameCacheEntry)
	}
	dg.pruneLabelNamesLocked()
	dg.labelNames[key] = labelNameCacheEntry{Name: name, ExpiresAt: time.Now().Add(chatNameCacheTTL)}
}

func (dg *Gateway) pruneLabelNamesLocked() {
	now := time.Now()
	for key, entry := range dg.labelNames {
		if !entry.ExpiresAt.After(now) {
			delete(dg.labelNames, key)
		}
	}
}

// sanitizeLabelValue strips control characters and quote characters and bounds
// length so an untrusted name cannot break the rendered label structure.
func sanitizeLabelValue(value string, maxRunes int) string {
	value = strings.Map(func(r rune) rune {
		switch {
		case r == '"' || r == '\'':
			return -1
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		default:
			return r
		}
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if maxRunes > 0 && utf8.RuneCountInString(value) > maxRunes {
		value = string([]rune(value)[:maxRunes])
	}
	return value
}
