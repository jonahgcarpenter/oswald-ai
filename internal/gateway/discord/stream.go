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
	if chunk.Type == agent.ChunkStatus {
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
			state.flushAnswer()
		}
	}
}

type discordStreamState struct {
	stream          *discordResponseStream
	active          strings.Builder
	messageID       string
	lastDisplay     string
	tools           []discordToolStatus
	replyUsed       bool
	replyMessageID  string
	answerStarted   bool
	previewDisabled bool
}

func (s *discordStreamState) consume(chunk agent.StreamChunk) {
	switch chunk.Type {
	case agent.ChunkThinking:
		if !s.answerStarted {
			s.showStatus()
		}
	case agent.ChunkContent:
		if chunk.Text == "" {
			return
		}
		s.active.WriteString(chunk.Text)
		if !s.answerStarted {
			s.answerStarted = true
			s.show(strings.TrimSpace(discordStreamCursor))
		}
	case agent.ChunkToolCall:
		s.active.Reset()
		s.answerStarted = false
		if chunk.Tool != nil && chunk.Tool.Name != "" {
			s.tools = append(s.tools, discordToolStatus{name: chunk.Tool.Name})
		}
		s.showStatus()
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
		s.showStatus()
	}
}

func (s *discordStreamState) showStatus() {
	if s.answerStarted {
		return
	}
	lines := []string{"Thinking..."}
	start := 0
	if len(s.tools) > discordToolStatusLineLimit {
		start = len(s.tools) - discordToolStatusLineLimit
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
	s.show(strings.Join(lines, "\n"))
}

func (s *discordStreamState) flushAnswer() {
	if !s.answerStarted || s.active.Len() == 0 {
		return
	}
	s.show(discordPreviewText(s.active.String()))
}

func (s *discordStreamState) show(content string) {
	if s.previewDisabled || content == "" || content == s.lastDisplay {
		return
	}
	var err error
	if s.messageID == "" {
		s.messageID, err = s.send(content)
	} else {
		err = s.stream.responder.gateway.editMessage(s.stream.responder.channelID, s.messageID, content)
	}
	if err != nil {
		s.previewDisabled = true
		s.stream.responder.gateway.log().Debug("gateway.stream.preview_failed", "discord lifecycle preview degraded", config.F("request_id", s.stream.responder.requestID), config.F("status", "degraded"), config.ErrorField(err))
		return
	}
	s.lastDisplay = content
	s.stream.responder.stopTypingIndicator()
}

func discordPreviewText(text string) string {
	limit := 2000 - utf8.RuneCountInString(discordStreamCursor) - utf8.RuneCountInString("\n```")
	chunks := splitMessage(text, limit)
	if len(chunks) == 0 {
		return strings.TrimSpace(discordStreamCursor)
	}
	preview := chunks[0]
	if strings.Count(preview, "```")%2 != 0 {
		preview += "\n```"
	}
	return preview + discordStreamCursor
}

func (s *discordStreamState) finish(response *agent.AgentResponse) error {
	responseText := response.Response
	chunks := splitMessage(responseText, 2000)
	log := s.stream.responder.gateway.log()
	log.Debug("gateway.response.prepared", "prepared discord response", config.F("request_id", s.stream.responder.requestID), config.F("chunk_count", len(chunks)), config.F("response_chars", len(responseText)), config.F("model", response.Model))

	sentCount := 0
	var lifecycleErr error
	if len(chunks) > 0 && s.messageID != "" {
		if err := s.stream.responder.gateway.editMessage(s.stream.responder.channelID, s.messageID, chunks[0]); err == nil {
			s.remember(s.messageID, chunks[0])
			sentCount++
			chunks = chunks[1:]
		} else {
			if deleteErr := s.delete(s.messageID, true); deleteErr != nil {
				lifecycleErr = fmt.Errorf("finalize discord lifecycle message: edit failed: %v; delete failed: %w", err, deleteErr)
				if s.messageID == s.replyMessageID {
					s.replyUsed = false
					s.replyMessageID = ""
				}
			}
			log.Debug("gateway.stream.final_edit_failed", "discord final lifecycle edit failed; sending authoritative response separately", config.F("request_id", s.stream.responder.requestID), config.F("status", "degraded"), config.ErrorField(err))
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
	return lifecycleErr
}

func (s *discordStreamState) fail(text string) error {
	var lifecycleErr error
	if s.messageID != "" {
		if err := s.stream.responder.gateway.editMessage(s.stream.responder.channelID, s.messageID, text); err == nil {
			s.remember(s.messageID, text)
			return nil
		} else if deleteErr := s.delete(s.messageID, true); deleteErr != nil {
			lifecycleErr = fmt.Errorf("replace discord lifecycle message with error: edit failed: %v; delete failed: %w", err, deleteErr)
			if s.messageID == s.replyMessageID {
				s.replyUsed = false
				s.replyMessageID = ""
			}
		}
	}
	messageID, err := s.send(text)
	if err != nil {
		return err
	}
	s.remember(messageID, text)
	return lifecycleErr
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
