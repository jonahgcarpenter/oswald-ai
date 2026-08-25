package discord

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

const (
	discordStreamEditInterval  = 800 * time.Millisecond
	discordStreamBufferRunes   = 24
	discordStreamCursor        = " ▉"
	discordToolStatusLineLimit = 12
)

type discordStreamEvent struct {
	chunk     *agent.StreamChunk
	response  *agent.AgentResponse
	errorText string
	result    chan error
}

type discordToolStatus struct {
	name       string
	done       bool
	failed     bool
	durationMS int64
}

type discordStaleMessage struct {
	id   string
	text string
}

type discordResponseStream struct {
	responder    *runtimeResponder
	events       chan discordStreamEvent
	startOnce    sync.Once
	editInterval time.Duration
}

func newDiscordResponseStream(responder *runtimeResponder) *discordResponseStream {
	return &discordResponseStream{
		responder:    responder,
		events:       make(chan discordStreamEvent, 128),
		editInterval: discordStreamEditInterval,
	}
}

func (s *discordResponseStream) start() {
	s.startOnce.Do(func() { go s.run() })
}

func (s *discordResponseStream) Push(chunk agent.StreamChunk) {
	if chunk.Type == agent.ChunkThinking || chunk.Type == agent.ChunkStatus {
		return
	}
	s.start()
	s.events <- discordStreamEvent{chunk: &chunk}
}

func (s *discordResponseStream) Finish(response *agent.AgentResponse) error {
	s.start()
	result := make(chan error, 1)
	s.events <- discordStreamEvent{response: response, result: result}
	return <-result
}

func (s *discordResponseStream) Fail(text string) error {
	s.start()
	result := make(chan error, 1)
	s.events <- discordStreamEvent{errorText: text, result: result}
	return <-result
}

func (s *discordResponseStream) run() {
	ticker := time.NewTicker(s.editInterval)
	defer ticker.Stop()

	state := discordStreamState{stream: s}
	for {
		select {
		case event := <-s.events:
			if event.chunk != nil {
				state.consume(*event.chunk)
				continue
			}
			if event.response != nil {
				event.result <- state.finish(event.response)
				return
			}
			if event.result != nil {
				event.result <- state.fail(event.errorText)
				return
			}
		case <-ticker.C:
			state.flushPreview(false)
		}
	}
}

type discordStreamState struct {
	stream             *discordResponseStream
	active             strings.Builder
	activeMessageID    string
	lastPreview        string
	toolMessageID      string
	tools              []discordToolStatus
	staleMessages      []discordStaleMessage
	replyUsed          bool
	replyMessageID     string
	previewDisabled    bool
	toolStatusDisabled bool
}

func (s *discordStreamState) consume(chunk agent.StreamChunk) {
	switch chunk.Type {
	case agent.ChunkContent:
		if chunk.Text == "" {
			return
		}
		s.active.WriteString(chunk.Text)
		if s.activeMessageID == "" && utf8.RuneCountInString(s.active.String()) >= discordStreamBufferRunes {
			s.flushPreview(false)
		}
	case agent.ChunkToolCall:
		s.finishActiveSegment()
		if chunk.Tool != nil && chunk.Tool.Name != "" {
			s.tools = append(s.tools, discordToolStatus{name: chunk.Tool.Name})
			s.flushToolStatus()
		}
	case agent.ChunkToolResult:
		if chunk.Tool == nil || chunk.Tool.Name == "" {
			return
		}
		for i := len(s.tools) - 1; i >= 0; i-- {
			if s.tools[i].name == chunk.Tool.Name && !s.tools[i].done {
				s.tools[i].done = true
				s.tools[i].failed = chunk.Tool.IsError
				s.tools[i].durationMS = chunk.Tool.DurationMS
				break
			}
		}
		s.flushToolStatus()
	}
}

func (s *discordStreamState) finishActiveSegment() {
	if s.active.Len() == 0 {
		return
	}
	text := s.active.String()
	if !s.previewDisabled {
		if err := s.deliverSegment(text); err != nil {
			s.stream.responder.gateway.log().Debug("gateway.stream.segment_finalize_failed", "discord streamed segment finalization degraded", config.F("request_id", s.stream.responder.requestID), config.F("status", "degraded"), config.ErrorField(err))
		}
	} else if s.activeMessageID != "" {
		if err := s.delete(s.activeMessageID, true); err != nil {
			s.staleMessages = append(s.staleMessages, discordStaleMessage{id: s.activeMessageID, text: text})
		}
	}
	s.active.Reset()
	s.activeMessageID = ""
	s.lastPreview = ""
	s.previewDisabled = false
}

func (s *discordStreamState) deliverSegment(text string) error {
	chunks := splitMessage(text, 2000)
	if len(chunks) == 0 {
		return nil
	}
	start := 0
	if s.activeMessageID != "" {
		if err := s.stream.responder.gateway.editMessage(s.stream.responder.channelID, s.activeMessageID, chunks[0]); err != nil {
			if deleteErr := s.delete(s.activeMessageID, true); deleteErr != nil {
				s.staleMessages = append(s.staleMessages, discordStaleMessage{id: s.activeMessageID, text: text})
			}
			return err
		}
		s.remember(s.activeMessageID, chunks[0])
		start = 1
	}
	for _, chunk := range chunks[start:] {
		messageID, err := s.send(chunk)
		if err != nil {
			return err
		}
		s.remember(messageID, chunk)
	}
	return nil
}

func (s *discordStreamState) flushPreview(finalize bool) {
	if s.previewDisabled || s.active.Len() == 0 {
		return
	}
	if s.activeMessageID == "" && s.hasStaleReply() {
		return
	}
	text := s.active.String()
	display := text
	if !finalize {
		display = discordPreviewText(text)
	}
	if display == s.lastPreview {
		return
	}

	var err error
	if s.activeMessageID == "" {
		s.activeMessageID, err = s.send(display)
	} else {
		err = s.stream.responder.gateway.editMessage(s.stream.responder.channelID, s.activeMessageID, display)
	}
	if err != nil {
		s.previewDisabled = true
		s.stream.responder.gateway.log().Debug("gateway.stream.preview_failed", "discord progressive preview degraded", config.F("request_id", s.stream.responder.requestID), config.F("status", "degraded"), config.ErrorField(err))
		return
	}
	s.lastPreview = display
	s.stream.responder.stopTypingIndicator()
}

func discordPreviewText(text string) string {
	limit := 2000 - utf8.RuneCountInString(discordStreamCursor) - utf8.RuneCountInString("\n```")
	chunks := splitMessage(text, limit)
	if len(chunks) == 0 {
		return ""
	}
	preview := chunks[0]
	if strings.Count(preview, "```")%2 != 0 {
		preview += "\n```"
	}
	return preview + discordStreamCursor
}

func (s *discordStreamState) flushToolStatus() {
	if s.toolStatusDisabled || len(s.tools) == 0 {
		return
	}
	start := 0
	if len(s.tools) > discordToolStatusLineLimit {
		start = len(s.tools) - discordToolStatusLineLimit
	}
	lines := make([]string, 0, len(s.tools)-start+1)
	if start > 0 {
		lines = append(lines, fmt.Sprintf("%d earlier tool calls completed.", start))
	}
	for _, tool := range s.tools[start:] {
		name := strings.ReplaceAll(tool.name, "`", "")
		switch {
		case !tool.done:
			lines = append(lines, fmt.Sprintf("Running `%s`...", name))
		case tool.failed:
			lines = append(lines, fmt.Sprintf("Failed `%s` (%d ms).", name, tool.durationMS))
		default:
			lines = append(lines, fmt.Sprintf("Completed `%s` (%d ms).", name, tool.durationMS))
		}
	}
	content := strings.Join(lines, "\n")
	var err error
	if s.toolMessageID == "" {
		s.toolMessageID, err = s.send(content)
	} else {
		err = s.stream.responder.gateway.editMessage(s.stream.responder.channelID, s.toolMessageID, content)
	}
	if err != nil {
		s.toolStatusDisabled = true
		if s.toolMessageID != "" {
			if s.delete(s.toolMessageID, true) == nil {
				s.toolMessageID = ""
			}
		}
		s.stream.responder.gateway.log().Debug("gateway.stream.tool_status_failed", "discord tool status delivery degraded", config.F("request_id", s.stream.responder.requestID), config.F("status", "degraded"), config.ErrorField(err))
		return
	}
	s.stream.responder.stopTypingIndicator()
}

func (s *discordStreamState) finish(response *agent.AgentResponse) error {
	responseText := response.Response
	chunks := splitMessage(responseText, 2000)
	log := s.stream.responder.gateway.log()
	log.Debug("gateway.response.prepared", "prepared discord response", config.F("request_id", s.stream.responder.requestID), config.F("chunk_count", len(chunks)), config.F("response_chars", len(responseText)), config.F("model", response.Model))

	for _, stale := range s.staleMessages {
		if stale.id == s.replyMessageID {
			if err := s.stream.responder.gateway.editMessage(s.stream.responder.channelID, stale.id, stale.text); err == nil {
				s.remember(stale.id, stale.text)
			} else {
				s.replyUsed = false
				s.replyMessageID = ""
			}
			continue
		}
		_ = s.delete(stale.id, false)
	}
	if s.toolStatusDisabled && s.toolMessageID != "" {
		s.toolStatusDisabled = false
		s.flushToolStatus()
	}

	sentCount := 0
	if len(chunks) > 0 && s.activeMessageID != "" {
		if err := s.stream.responder.gateway.editMessage(s.stream.responder.channelID, s.activeMessageID, chunks[0]); err == nil {
			s.remember(s.activeMessageID, chunks[0])
			sentCount++
			chunks = chunks[1:]
		} else {
			if deleteErr := s.delete(s.activeMessageID, true); deleteErr != nil && s.activeMessageID == s.replyMessageID {
				s.replyUsed = false
				s.replyMessageID = ""
			}
			log.Debug("gateway.stream.final_edit_failed", "discord final preview edit failed; sending authoritative response separately", config.F("request_id", s.stream.responder.requestID), config.F("status", "degraded"), config.ErrorField(err))
		}
	}

	for i, chunk := range chunks {
		messageID, err := s.send(chunk)
		if err != nil {
			log.Error("gateway.send.failed", "failed to send discord response chunk", config.F("request_id", s.stream.responder.requestID), config.F("chunk_index", i+1), config.ErrorField(err))
			return err
		}
		s.remember(messageID, chunk)
		sentCount++
	}
	log.Debug("gateway.response.sent", "sent discord response", config.F("request_id", s.stream.responder.requestID), config.F("chunk_count", sentCount), config.F("status", "ok"))
	return nil
}

func (s *discordStreamState) fail(text string) error {
	s.finishActiveSegment()
	for _, stale := range s.staleMessages {
		_ = s.delete(stale.id, false)
	}
	messageID, err := s.send(text)
	if err == nil {
		s.remember(messageID, text)
	}
	return err
}

func (s *discordStreamState) send(content string) (string, error) {
	replyToID := ""
	if !s.replyUsed {
		replyToID = s.stream.responder.replyToID
	}
	messageID, err := s.stream.responder.gateway.sendMessage(s.stream.responder.channelID, content, replyToID)
	if err == nil {
		if !s.replyUsed {
			s.replyMessageID = messageID
		}
		s.replyUsed = true
	}
	return messageID, err
}

func (s *discordStreamState) delete(messageID string, releaseReply bool) error {
	err := s.stream.responder.gateway.deleteMessage(s.stream.responder.channelID, messageID)
	if err == nil && releaseReply && messageID == s.replyMessageID {
		s.replyUsed = false
		s.replyMessageID = ""
	}
	return err
}

func (s *discordStreamState) hasStaleReply() bool {
	for _, stale := range s.staleMessages {
		if stale.id == s.replyMessageID {
			return true
		}
	}
	return false
}

func (s *discordStreamState) remember(messageID, text string) {
	if messageID == "" {
		return
	}
	r := s.stream.responder
	r.gateway.rememberReply(messageID, replyContext{
		SessionKey: r.sessionKey, ChannelID: r.channelID, SenderID: r.authorID,
		DisplayName: "Oswald", Text: text, IsFromBot: true, CreatedAt: time.Now(),
	})
}
