package imessage

import (
	"sort"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

const (
	coalesceQuietDefault   = 1000 * time.Millisecond
	coalesceMaxHoldDefault = 3000 * time.Millisecond
	urlBalloonProvider     = "com.apple.messages.URLBalloonProvider"
)

// coalesceEntry preserves arrival order for stable merging.
type coalesceEntry struct {
	msg        webhookMessage
	requestID  string
	receivedAt time.Time
	order      int
}

type coalesceBatch struct {
	entries []coalesceEntry
	seen    map[string]struct{}
	firstAt time.Time
	timer   *time.Timer
}

func (g *Gateway) coalesceQuietDuration() time.Duration {
	if g.coalesceQuiet > 0 {
		return g.coalesceQuiet
	}
	return coalesceQuietDefault
}

func (g *Gateway) coalesceMaxHoldDuration() time.Duration {
	if g.coalesceMaxHold > 0 {
		return g.coalesceMaxHold
	}
	return coalesceMaxHoldDefault
}

// enqueueCoalesced groups rapid BlueBubbles parts from one sender/chat so a
// multi-part iMessage flushes as a single processReceivedMessage call.
func (g *Gateway) enqueueCoalesced(msg webhookMessage, requestID string, receivedAt time.Time) {
	chat := msg.primaryChat()
	sender := strings.TrimSpace(msg.Handle.Address)
	if chat.GUID == "" || sender == "" {
		go g.processReceivedMessage(msg, requestID, receivedAt)
		return
	}
	key := chat.GUID + "\x00" + sender

	g.coalesceMu.Lock()
	if g.coalesceBatches == nil {
		g.coalesceBatches = make(map[string]*coalesceBatch)
	}
	batch := g.coalesceBatches[key]
	if batch == nil {
		batch = &coalesceBatch{seen: make(map[string]struct{}), firstAt: time.Now()}
		g.coalesceBatches[key] = batch
	}
	if strings.TrimSpace(msg.GUID) != "" {
		if _, dup := batch.seen[msg.GUID]; dup {
			g.coalesceMu.Unlock()
			return
		}
		batch.seen[msg.GUID] = struct{}{}
	}
	batch.entries = append(batch.entries, coalesceEntry{msg: msg, requestID: requestID, receivedAt: receivedAt, order: len(batch.entries)})
	if batch.timer != nil {
		batch.timer.Stop()
	}
	quiet := g.coalesceQuietDuration()
	maxHold := g.coalesceMaxHoldDuration()
	delay := quiet
	if remaining := maxHold - time.Since(batch.firstAt); remaining <= 0 {
		entries := batch.entries
		delete(g.coalesceBatches, key)
		g.coalesceMu.Unlock()
		g.flushCoalesced(entries)
		return
	} else if remaining < delay {
		delay = remaining
	}
	batch.timer = time.AfterFunc(delay, func() { g.flushCoalesceKey(key) })
	g.coalesceMu.Unlock()
}

func (g *Gateway) flushCoalesceKey(key string) {
	g.coalesceMu.Lock()
	batch := g.coalesceBatches[key]
	if batch == nil {
		g.coalesceMu.Unlock()
		return
	}
	delete(g.coalesceBatches, key)
	entries := batch.entries
	g.coalesceMu.Unlock()
	g.flushCoalesced(entries)
}

func (g *Gateway) flushCoalesced(entries []coalesceEntry) {
	if len(entries) == 0 {
		return
	}
	merged, requestID, receivedAt := mergeCoalescedEntries(entries)
	g.log().With(config.F("request_id", requestID)).Debug(
		"gateway.message.coalesced",
		"merged imessage parts",
		config.F("part_count", len(entries)),
		config.F("attachment_count", len(merged.Attachments)),
		config.F("message_chars", len(merged.Text)),
	)
	go g.processReceivedMessage(merged, requestID, receivedAt)
}

// mergeCoalescedEntries orders parts by creation time and drops link-unfurl
// preview payloads, keeping the URL text and real attachments.
func mergeCoalescedEntries(entries []coalesceEntry) (webhookMessage, string, time.Time) {
	sorted := append([]coalesceEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ai, aj := sorted[i].msg.DateCreated, sorted[j].msg.DateCreated
		if ai != nil && aj != nil && *ai != *aj {
			return *ai < *aj
		}
		if ai != nil && aj == nil {
			return true
		}
		if ai == nil && aj != nil {
			return false
		}
		if sorted[i].msg.OriginalROWID != sorted[j].msg.OriginalROWID {
			return sorted[i].msg.OriginalROWID < sorted[j].msg.OriginalROWID
		}
		return sorted[i].order < sorted[j].order
	})

	base := sorted[0].msg
	texts := make([]string, 0, len(sorted))
	attachments := make([]attachment, 0)
	seenAttachments := make(map[string]struct{})
	for _, entry := range sorted {
		if text := strings.TrimSpace(entry.msg.Text); text != "" {
			texts = append(texts, text)
		}
		if entry.msg.BalloonBundleID == urlBalloonProvider {
			continue
		}
		for _, att := range entry.msg.Attachments {
			if isPreviewAttachment(att) {
				continue
			}
			if strings.TrimSpace(att.GUID) != "" {
				if _, dup := seenAttachments[att.GUID]; dup {
					continue
				}
				seenAttachments[att.GUID] = struct{}{}
			}
			attachments = append(attachments, att)
		}
	}
	merged := base
	merged.Text = strings.Join(texts, "\n")
	merged.Attachments = attachments
	for _, entry := range sorted {
		if strings.TrimSpace(merged.ReplyToGUID) == "" && strings.TrimSpace(entry.msg.ReplyToGUID) != "" {
			merged.ReplyToGUID = entry.msg.ReplyToGUID
		}
		if strings.TrimSpace(merged.ThreadOriginatorGUID) == "" && strings.TrimSpace(entry.msg.ThreadOriginatorGUID) != "" {
			merged.ThreadOriginatorGUID = entry.msg.ThreadOriginatorGUID
			merged.ThreadOriginatorPart = entry.msg.ThreadOriginatorPart
		}
	}
	// Earliest GUID wins for idempotency; request correlation uses arrival.
	merged.GUID = strings.TrimSpace(sorted[0].msg.GUID)
	return merged, entries[0].requestID, entries[0].receivedAt
}

func isPreviewAttachment(att attachment) bool {
	if strings.HasSuffix(att.TransferName, ".pluginPayloadAttachment") {
		return true
	}
	return strings.TrimSpace(att.MimeType) == "" && strings.HasPrefix(strings.TrimSpace(att.UTI), "dyn.")
}
