package imessage

import (
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// defaultBackupTypingStopAfter is the delay before the precautionary typing
// DELETE after a terminal send. The BlueBubbles server is assumed to clear
// the indicator when the reply is sent; the backup only catches the stuck
// case. Tests override it per gateway via typingBackupAfter.
const defaultBackupTypingStopAfter = 2500 * time.Millisecond

// backupAfter returns the configured backup stop delay for this gateway.
func (g *Gateway) backupAfter() time.Duration {
	if g != nil && g.typingBackupAfter > 0 {
		return g.typingBackupAfter
	}
	return defaultBackupTypingStopAfter
}

// trackTypingActive records a chat with a live indicator so shutdown can
// deliberately clear it. Over-inclusion is harmless: extra DELETEs are idempotent.
func (g *Gateway) trackTypingActive(chatGUID string) {
	if chatGUID == "" {
		return
	}
	g.typingMu.Lock()
	if g.typingActive == nil {
		g.typingActive = make(map[string]struct{})
	}
	g.typingActive[chatGUID] = struct{}{}
	g.typingMu.Unlock()
}

// cancelPendingBackup drops a scheduled backup stop without sending it.
// A new POST for the same chat supersedes the previous turn's backup so
// back-to-back turns never flicker the indicator off.
func (g *Gateway) cancelPendingBackup(chatGUID string) {
	if chatGUID == "" {
		return
	}
	g.typingMu.Lock()
	if timer, ok := g.typingTimers[chatGUID]; ok && timer != nil {
		timer.Stop()
		delete(g.typingTimers, chatGUID)
	}
	g.typingMu.Unlock()
}

// scheduleBackupTypingStop arms the single precautionary DELETE for a chat,
// replacing any pending one. Steer and queue follow-ups never call this —
// only the active turn's terminal send, deliberate cancel, or shutdown stops.
func (g *Gateway) scheduleBackupTypingStop(chatGUID, requestID string) {
	if chatGUID == "" {
		return
	}
	g.typingMu.Lock()
	delete(g.typingActive, chatGUID)
	if timer, ok := g.typingTimers[chatGUID]; ok && timer != nil {
		timer.Stop()
	}
	if g.typingTimers == nil {
		g.typingTimers = make(map[string]*time.Timer)
	}
	log := g.log().With(config.F("request_id", requestID))
	g.typingTimers[chatGUID] = time.AfterFunc(g.backupAfter(), func() {
		g.typingMu.Lock()
		delete(g.typingTimers, chatGUID)
		g.typingMu.Unlock()
		if err := g.stopTyping(chatGUID, log); err != nil {
			log.Debug("gateway.typing.backup_stop_failed", "precautionary typing delete failed", config.F("status", "degraded"), config.ErrorField(err))
			return
		}
		log.Debug("gateway.typing.backup_stop", "sent precautionary typing delete after send", config.F("status", "ok"))
	})
	g.typingMu.Unlock()
}

// stopTypingNow sends one deliberate DELETE for a chat. Used for cancels and
// shutdown, where no reply was sent so the server cannot have auto-cleared.
func (g *Gateway) stopTypingNow(chatGUID string, scoped ...*config.Logger) error {
	g.cancelPendingBackup(chatGUID)
	return g.stopTyping(chatGUID, scoped...)
}

// StopOutbound sends deliberate typing DELETEs for in-flight chats so a
// shutdown never leaves a stuck indicator. Pending backups are flushed now
// instead of firing mid-shutdown. Best-effort and never fails shutdown.
func (g *Gateway) StopOutbound() {
	g.typingMu.Lock()
	seen := make(map[string]struct{})
	var chats []string
	for guid := range g.typingActive {
		if _, dup := seen[guid]; !dup {
			seen[guid] = struct{}{}
			chats = append(chats, guid)
		}
	}
	for guid, timer := range g.typingTimers {
		if timer != nil {
			timer.Stop()
		}
		if _, dup := seen[guid]; !dup {
			seen[guid] = struct{}{}
			chats = append(chats, guid)
		}
	}
	g.typingActive = make(map[string]struct{})
	g.typingTimers = make(map[string]*time.Timer)
	g.typingMu.Unlock()
	log := g.log()
	for _, guid := range chats {
		if err := g.stopTyping(guid, log); err != nil {
			log.Debug("gateway.typing.shutdown_stop_failed", "failed to clear typing on shutdown", config.F("status", "degraded"), config.ErrorField(err))
		}
	}
	if len(chats) > 0 {
		log.Debug("gateway.typing.shutdown_stop", "cleared in-flight typing indicators on shutdown", config.F("chat_count", len(chats)))
	}
}
