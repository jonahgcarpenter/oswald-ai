package discord

import (
	"regexp"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/gateway/routing"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
)

const replyIndexTTL = time.Hour

func (dg *Gateway) rememberReply(messageID string, ctx replyContext) {
	if messageID == "" {
		return
	}

	dg.replyMu.Lock()
	if dg.replyIndex == nil {
		dg.replyIndex = make(map[string]replyContext)
	}
	dg.pruneReplyIndexLocked()
	dg.replyIndex[messageID] = ctx
	dg.replyMu.Unlock()
}

func (dg *Gateway) lookupReply(messageID string) (replyContext, bool) {
	if messageID == "" {
		return replyContext{}, false
	}

	dg.replyMu.Lock()
	dg.pruneReplyIndexLocked()
	ctx, ok := dg.replyIndex[messageID]
	dg.replyMu.Unlock()

	return ctx, ok
}

func (dg *Gateway) pruneReplyIndexLocked() int {
	cutoff := time.Now().Add(-replyIndexTTL)
	pruned := 0
	for id, ctx := range dg.replyIndex {
		if ctx.CreatedAt.Before(cutoff) {
			delete(dg.replyIndex, id)
			pruned++
		}
	}
	return pruned
}

func (dg *Gateway) resolveReplyContext(msg MessageCreate, emojiRE *regexp.Regexp, currentImages []llm.InputImage, requestID, userID string) *routing.ReplyContext {
	log := dg.log().With(config.F("request_id", requestID))
	referenced := msg.ReferencedMessage
	if referenced == nil {
		return nil
	}

	if cached, ok := dg.lookupReply(referenced.ID); ok {
		reply := &routing.ReplyContext{
			SenderName: strings.TrimSpace(cached.DisplayName),
			Text:       strings.TrimSpace(cached.Text),
			IsFromBot:  cached.IsFromBot,
		}
		if reply.SenderName == "" && cached.IsFromBot {
			reply.SenderName = "Oswald"
		}
		if len(cached.Attachments) > 0 {
			remainingImageSlots := media.MaxImagesPerRequest - len(currentImages)
			if remainingImageSlots > 0 {
				reply.Images, reply.Unsupported = dg.loadImagesLimit(cached.Attachments, remainingImageSlots, log)
			} else {
				reply.Unsupported = unsupportedImageLabels(cached.Attachments)
			}
			if mediaRefs, mediaUnsupported := dg.loadMedia(cached.Attachments, userID, log); len(mediaRefs) > 0 || len(mediaUnsupported) > 0 {
				reply.Documents = append(reply.Documents, mediaRefs...)
				reply.Unsupported = append(reply.Unsupported, mediaUnsupported...)
			}
		}
		if len(cached.Embeds) > 0 {
			remainingImageSlots := media.MaxImagesPerRequest - len(currentImages) - len(reply.Images)
			embedImages, embedMedia, embedUnsupported := dg.loadEmbedImagesLimit(cached.Embeds, remainingImageSlots, userID, log)
			reply.Images = append(reply.Images, embedImages...)
			reply.Documents = append(reply.Documents, embedMedia...)
			reply.Unsupported = append(reply.Unsupported, embedUnsupported...)
			if len(embedImages) > 0 || len(embedMedia) > 0 {
				reply.Text = stripEmbedURLsFromText(reply.Text, cached.Embeds)
			}
		}
		log.Debug("gateway.reply_context.applied", "applied discord cached reply context", config.F("request_id", requestID), config.F("chat_id", msg.ChannelID), config.F("is_bot_reply", cached.IsFromBot), config.F("reply_image_count", len(reply.Images)))
		return reply
	}

	replyName := strings.TrimSpace(referenced.Author.Username)
	if replyName == "" && referenced.Author.ID == dg.BotID {
		replyName = "Oswald"
	}
	quotedContent := strings.TrimSpace(emojiRE.ReplaceAllString(referenced.Content, ":$1:"))
	reply := &routing.ReplyContext{
		SenderName: replyName,
		Text:       quotedContent,
		IsFromBot:  referenced.Author.ID == dg.BotID,
	}
	if len(referenced.Attachments) > 0 {
		remainingImageSlots := media.MaxImagesPerRequest - len(currentImages)
		if remainingImageSlots > 0 {
			reply.Images, reply.Unsupported = dg.loadImagesLimit(referenced.Attachments, remainingImageSlots, log)
		} else {
			reply.Unsupported = unsupportedImageLabels(referenced.Attachments)
		}
		if mediaRefs, mediaUnsupported := dg.loadMedia(referenced.Attachments, userID, log); len(mediaRefs) > 0 || len(mediaUnsupported) > 0 {
			reply.Documents = append(reply.Documents, mediaRefs...)
			reply.Unsupported = append(reply.Unsupported, mediaUnsupported...)
		}
	}
	if len(referenced.Embeds) > 0 {
		remainingImageSlots := media.MaxImagesPerRequest - len(currentImages) - len(reply.Images)
		embedImages, embedMedia, embedUnsupported := dg.loadEmbedImagesLimit(referenced.Embeds, remainingImageSlots, userID, log)
		reply.Images = append(reply.Images, embedImages...)
		reply.Documents = append(reply.Documents, embedMedia...)
		reply.Unsupported = append(reply.Unsupported, embedUnsupported...)
		if len(embedImages) > 0 || len(embedMedia) > 0 {
			reply.Text = stripEmbedURLsFromText(reply.Text, referenced.Embeds)
		}
	}
	if quotedContent == "" && len(reply.Images) == 0 && len(reply.Unsupported) == 0 {
		if fetched, ok := dg.fetchMessage(msg.ChannelID, referenced.ID, requestID); ok {
			reply.SenderName = strings.TrimSpace(fetched.Author.Username)
			if reply.SenderName == "" && fetched.Author.ID == dg.BotID {
				reply.SenderName = "Oswald"
			}
			reply.Text = strings.TrimSpace(emojiRE.ReplaceAllString(fetched.Content, ":$1:"))
			reply.IsFromBot = fetched.Author.ID == dg.BotID
			if len(fetched.Attachments) > 0 {
				remainingImageSlots := media.MaxImagesPerRequest - len(currentImages)
				if remainingImageSlots > 0 {
					reply.Images, reply.Unsupported = dg.loadImagesLimit(fetched.Attachments, remainingImageSlots, log)
				} else {
					reply.Unsupported = unsupportedImageLabels(fetched.Attachments)
				}
				if mediaRefs, mediaUnsupported := dg.loadMedia(fetched.Attachments, userID, log); len(mediaRefs) > 0 || len(mediaUnsupported) > 0 {
					reply.Documents = append(reply.Documents, mediaRefs...)
					reply.Unsupported = append(reply.Unsupported, mediaUnsupported...)
				}
			}
			if len(fetched.Embeds) > 0 {
				remainingImageSlots := media.MaxImagesPerRequest - len(currentImages) - len(reply.Images)
				embedImages, embedMedia, embedUnsupported := dg.loadEmbedImagesLimit(fetched.Embeds, remainingImageSlots, userID, log)
				reply.Images = append(reply.Images, embedImages...)
				reply.Documents = append(reply.Documents, embedMedia...)
				reply.Unsupported = append(reply.Unsupported, embedUnsupported...)
				if len(embedImages) > 0 || len(embedMedia) > 0 {
					reply.Text = stripEmbedURLsFromText(reply.Text, fetched.Embeds)
				}
			}
		}
	}
	if strings.TrimSpace(reply.Text) == "" && len(reply.Images) == 0 && len(reply.Documents) == 0 && len(reply.Unsupported) == 0 {
		reply.IsUnavailable = true
	}
	status := "ok"
	if reply.IsUnavailable {
		status = "degraded"
	}
	log.Debug("gateway.reply_context.applied", "applied discord reply context", config.F("request_id", requestID), config.F("chat_id", msg.ChannelID), config.F("is_bot_reply", reply.IsFromBot), config.F("reply_image_count", len(reply.Images)), config.F("status", status))
	return reply
}
