package imessage

import (
	"encoding/json"
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

// chatLabel builds the untrusted conversation label shown in the system prompt.
// Dynamic names are sanitized; the surrounding structure is literal.
func (g *Gateway) chatLabel(chat messageChat, displayName string, isGroup bool, scoped ...*config.Logger) string {
	user := sanitizeLabelValue(displayName, chatLabelMaxRunes)
	if !isGroup {
		if user == "" {
			return `"DM"`
		}
		return `"DM with ` + user + `"`
	}
	if name := g.chatName(chat.GUID, scoped...); name != "" {
		return `group chat "` + name + `"`
	}
	return `"group chat"`
}

func (g *Gateway) chatName(chatGUID string, scoped ...*config.Logger) string {
	chatGUID = strings.TrimSpace(chatGUID)
	if chatGUID == "" {
		return ""
	}
	if name, ok := g.cachedChatName(chatGUID); ok {
		return name
	}
	name := g.fetchChatName(chatGUID, scoped...)
	if name != "" {
		g.cacheChatName(chatGUID, name)
	}
	return name
}

func (g *Gateway) fetchChatName(chatGUID string, scoped ...*config.Logger) string {
	log := g.log(scoped...)
	endpoint, err := buildBlueBubblesChatEndpoint(g.BlueBubblesURL, chatGUID, g.BlueBubblesPassword)
	if err != nil {
		log.Debug("gateway.chat_label.failed", "failed to build BlueBubbles chat request", config.F("status", "degraded"), config.ErrorField(err))
		return ""
	}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		log.Debug("gateway.chat_label.failed", "failed to build BlueBubbles chat request", config.F("status", "degraded"), config.ErrorField(err))
		return ""
	}
	resp, err := g.httpClient().Do(req)
	if err != nil {
		log.Debug("gateway.chat_label.failed", "BlueBubbles chat request failed", config.F("status", "degraded"), config.ErrorField(err))
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Debug("gateway.chat_label.failed", "BlueBubbles chat request failed", config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
		return ""
	}
	var result chatInfoResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&result); err != nil {
		log.Debug("gateway.chat_label.failed", "failed to decode BlueBubbles chat response", config.F("status", "degraded"), config.ErrorField(err))
		return ""
	}
	return sanitizeLabelValue(result.Data.DisplayName, chatLabelMaxRunes)
}

func (g *Gateway) cachedChatName(chatGUID string) (string, bool) {
	g.chatNameMu.Lock()
	defer g.chatNameMu.Unlock()
	g.pruneChatNamesLocked()
	entry, ok := g.chatNames[chatGUID]
	if !ok {
		return "", false
	}
	return entry.Name, true
}

func (g *Gateway) cacheChatName(chatGUID, name string) {
	g.chatNameMu.Lock()
	defer g.chatNameMu.Unlock()
	if g.chatNames == nil {
		g.chatNames = make(map[string]chatNameCacheEntry)
	}
	g.pruneChatNamesLocked()
	g.chatNames[chatGUID] = chatNameCacheEntry{Name: name, ExpiresAt: time.Now().Add(chatNameCacheTTL)}
}

func (g *Gateway) pruneChatNamesLocked() {
	now := time.Now()
	for chatGUID, entry := range g.chatNames {
		if !entry.ExpiresAt.After(now) {
			delete(g.chatNames, chatGUID)
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
