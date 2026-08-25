package discord

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

const (
	discordStreamEditInterval     = 800 * time.Millisecond
	discordStreamCursor           = " ▉"
	discordThinkingRenderLimit    = 2000
	discordThinkingRetentionLimit = 8000
	discordToolArgumentValueLimit = 120
	discordToolStatusRenderLimit  = 2000
)

type discordStreamEvent struct {
	chunk     *agent.StreamChunk
	response  *agent.AgentResponse
	errorText string
	result    chan error
}

type discordToolStatus struct {
	name      string
	running   string
	completed string
	failed    string
	done      bool
	isError   bool
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
			state.flush()
		}
	}
}

type discordStreamState struct {
	stream          *discordResponseStream
	active          strings.Builder
	thinking        string
	frozenThinking  string
	messageID       string
	lastDisplay     string
	tool            *discordToolStatus
	replyUsed       bool
	replyMessageID  string
	answerStarted   bool
	thinkingStarted bool
	thinkingDirty   bool
	thinkingTrimmed bool
	frozenTrimmed   bool
	previewDisabled bool
}

func (s *discordStreamState) consume(chunk agent.StreamChunk) {
	switch chunk.Type {
	case agent.ChunkThinking:
		if s.answerStarted {
			return
		}
		if chunk.Text == "" {
			return
		}
		firstThinkingChunk := !s.thinkingStarted
		if !s.thinkingStarted {
			s.thinking = ""
			s.frozenThinking = ""
			s.thinkingTrimmed = false
			s.frozenTrimmed = false
			s.thinkingStarted = true
			s.tool = nil
		}
		s.appendThinking(chunk.Text)
		if firstThinkingChunk {
			s.showThinking()
		}
	case agent.ChunkContent:
		if chunk.Text == "" {
			return
		}
		s.active.WriteString(chunk.Text)
		if !s.answerStarted {
			s.answerStarted = true
			s.thinking = ""
			s.frozenThinking = ""
			s.thinkingStarted = false
			s.thinkingDirty = false
			s.thinkingTrimmed = false
			s.frozenTrimmed = false
			s.tool = nil
			s.show(strings.TrimSpace(discordStreamCursor))
		}
	case agent.ChunkToolCall:
		s.active.Reset()
		s.answerStarted = false
		if s.thinkingStarted {
			s.frozenThinking = latestThinkingParagraph(s.thinking)
			s.frozenTrimmed = s.thinkingTrimmed
		} else {
			s.frozenThinking = ""
			s.frozenTrimmed = false
		}
		s.thinking = ""
		s.thinkingStarted = false
		s.thinkingDirty = false
		s.thinkingTrimmed = false
		if chunk.Tool != nil && chunk.Tool.Name != "" {
			status := discordToolStatusFor(chunk.Tool)
			s.tool = &status
		}
		s.showTool()
	case agent.ChunkToolResult:
		if chunk.Tool == nil || chunk.Tool.Name == "" {
			return
		}
		if s.tool == nil || s.tool.name != chunk.Tool.Name {
			status := discordToolStatusFor(chunk.Tool)
			s.tool = &status
		}
		s.tool.done = true
		s.tool.isError = chunk.Tool.IsError
		s.frozenThinking = ""
		s.frozenTrimmed = false
		s.showTool()
	}
}

func (s *discordStreamState) appendThinking(text string) {
	s.thinking += text
	runes := []rune(s.thinking)
	if len(runes) > discordThinkingRetentionLimit {
		s.thinking = string(runes[len(runes)-discordThinkingRetentionLimit:])
		s.thinkingTrimmed = true
	}
	s.thinkingDirty = true
}

func (s *discordStreamState) flush() {
	if s.answerStarted {
		s.flushAnswer()
		return
	}
	if s.thinkingDirty {
		s.showThinking()
	}
}

func (s *discordStreamState) showThinking() {
	if s.answerStarted || !s.thinkingStarted {
		return
	}
	paragraph := latestThinkingParagraph(s.thinking)
	if paragraph == "" {
		return
	}
	s.show(formatThinkingParagraph(paragraph, s.thinkingTrimmed, true, discordThinkingRenderLimit))
	s.thinkingDirty = false
}

func (s *discordStreamState) showTool() {
	if s.answerStarted || s.tool == nil {
		return
	}
	status := s.tool.running
	if s.tool.done && s.tool.isError {
		status = s.tool.failed
	} else if s.tool.done {
		status = s.tool.completed
	}
	if !s.tool.done && s.frozenThinking != "" {
		status = composeRunningToolStatus(s.frozenThinking, s.frozenTrimmed, status)
	}
	s.show(truncateDiscordText(status, discordToolStatusRenderLimit))
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
	limit := 2000 - discordContentLength(discordStreamCursor) - discordContentLength("\n```")
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

func latestThinkingParagraph(text string) string {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	lines := strings.Split(normalized, "\n")
	current := make([]string, 0, len(lines))
	last := ""
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			if paragraph := strings.TrimSpace(strings.Join(current, "\n")); paragraph != "" {
				last = paragraph
			}
			current = current[:0]
			continue
		}
		current = append(current, line)
	}
	if paragraph := strings.TrimSpace(strings.Join(current, "\n")); paragraph != "" {
		return paragraph
	}
	return last
}

func formatThinkingParagraph(paragraph string, omitted, cursor bool, limit int) string {
	paragraph = sanitizeDiscordDisplayText(paragraph, true)
	if paragraph == "" || limit <= 0 {
		return ""
	}
	render := func(text string, showOmission bool) string {
		lines := make([]string, 0, strings.Count(text, "\n")+2)
		if showOmission {
			lines = append(lines, "-# [earlier part omitted]")
		}
		for _, line := range strings.Split(text, "\n") {
			lines = append(lines, "-# "+line)
		}
		if cursor && len(lines) > 0 {
			lines[len(lines)-1] += discordStreamCursor
		}
		return strings.Join(lines, "\n")
	}

	if full := render(paragraph, omitted); discordContentLength(full) <= limit {
		return full
	}

	runes := []rune(paragraph)
	low, high := 0, len(runes)-1
	best := ""
	for low <= high {
		mid := low + (high-low)/2
		candidate := strings.TrimLeftFunc(string(runes[mid:]), unicode.IsSpace)
		rendered := render(candidate, true)
		if discordContentLength(rendered) <= limit {
			best = rendered
			high = mid - 1
		} else {
			low = mid + 1
		}
	}
	return best
}

func composeRunningToolStatus(paragraph string, omitted bool, status string) string {
	status = truncateDiscordText(status, discordToolStatusRenderLimit)
	separator := "\n\n"
	thinkingBudget := discordToolStatusRenderLimit - discordContentLength(status) - discordContentLength(separator)
	thinking := formatThinkingParagraph(paragraph, omitted, false, thinkingBudget)
	if thinking == "" {
		return status
	}
	return thinking + separator + status
}

func discordToolStatusFor(tool *agent.ToolStreamPayload) discordToolStatus {
	name := sanitizeDiscordInline(tool.Name)
	query := toolStringArgument(tool.Arguments, "query")
	if tool.WebSearch != nil && tool.WebSearch.Query != "" {
		query = tool.WebSearch.Query
	}

	switch tool.Name {
	case "web.search":
		return actionToolStatus(tool.Name, "Searching the web for "+quoteToolDetail(query), "Searched the web for "+quoteToolDetail(query), "Web search failed for "+quoteToolDetail(query))
	case "time.current":
		timezone := toolStringArgument(tool.Arguments, "timezone")
		if timezone == "" {
			timezone = "the requested timezone"
		}
		return actionToolStatus(tool.Name, "Checking the time in `"+sanitizeDiscordInline(timezone)+"`", "Checked the time in `"+sanitizeDiscordInline(timezone)+"`", "Time lookup failed for `"+sanitizeDiscordInline(timezone)+"`")
	case "user_memory_search":
		detail := memoryFilterDetail(tool.Arguments)
		if query != "" {
			detail = " for " + quoteToolDetail(query) + detail
		}
		return actionToolStatus(tool.Name, "Searching your memories"+detail, "Searched your memories"+detail, "Memory search failed"+detail)
	case "user_memory_list":
		detail := memoryFilterDetail(tool.Arguments)
		return actionToolStatus(tool.Name, "Listing your memories"+detail, "Listed your memories"+detail, "Memory listing failed"+detail)
	case "session_transcript_search":
		return actionToolStatus(tool.Name, "Searching this conversation for "+quoteToolDetail(query), "Searched this conversation for "+quoteToolDetail(query), "Conversation search failed for "+quoteToolDetail(query))
	case "global_memory_search":
		if tool.GlobalMemory != nil && tool.GlobalMemory.Query != "" {
			query = tool.GlobalMemory.Query
		}
		return actionToolStatus(tool.Name, "Searching Oswald memory for "+quoteToolDetail(query), "Searched Oswald memory for "+quoteToolDetail(query), "Oswald memory search failed for "+quoteToolDetail(query))
	default:
		detail := formatMCPToolArguments(tool.Arguments)
		return actionToolStatus(tool.Name, "Running `"+name+"`"+detail, "Completed `"+name+"`"+detail, "Failed `"+name+"`"+detail)
	}
}

func actionToolStatus(name, running, completed, failed string) discordToolStatus {
	return discordToolStatus{
		name:      name,
		running:   truncateDiscordText(running+"...", discordToolStatusRenderLimit),
		completed: truncateDiscordText(completed+".", discordToolStatusRenderLimit),
		failed:    truncateDiscordText(failed+".", discordToolStatusRenderLimit),
	}
}

func memoryFilterDetail(args map[string]interface{}) string {
	filters := make([]string, 0, 2)
	if scope := toolStringArgument(args, "scope"); scope != "" {
		filters = append(filters, sanitizeDiscordInline(scope))
	}
	if category := toolStringArgument(args, "category"); category != "" {
		filters = append(filters, sanitizeDiscordInline(category))
	}
	if len(filters) == 0 {
		return ""
	}
	return " in " + strings.Join(filters, "/")
}

func toolStringArgument(args map[string]interface{}, key string) string {
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}

func quoteToolDetail(value string) string {
	value = sanitizeDiscordDisplayText(value, false)
	if value == "" {
		value = "the requested information"
	}
	return strconv.Quote(truncateDiscordText(value, discordToolArgumentValueLimit))
}

func formatMCPToolArguments(args map[string]interface{}) string {
	keys := make([]string, 0, len(args))
	for key := range args {
		if !isSensitiveToolArgument(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value, ok := formatToolArgumentValue(args[key])
		if !ok {
			continue
		}
		parts = append(parts, sanitizeDiscordInline(key)+"="+value)
	}
	if len(parts) == 0 {
		return ""
	}
	return " with " + strings.Join(parts, ", ")
}

func formatToolArgumentValue(value interface{}) (string, bool) {
	switch typed := value.(type) {
	case string:
		return quoteToolDetail(typed), true
	case bool:
		return strconv.FormatBool(typed), true
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32), true
	case int:
		return strconv.Itoa(typed), true
	case int64:
		return strconv.FormatInt(typed, 10), true
	case []string:
		values := make([]string, 0, min(len(typed), 4))
		for i, item := range typed {
			if i == 4 {
				break
			}
			values = append(values, quoteToolDetail(item))
		}
		return "[" + strings.Join(values, ", ") + "]", true
	case []interface{}:
		values := make([]string, 0, min(len(typed), 4))
		for i, item := range typed {
			if i == 4 {
				break
			}
			formatted, ok := formatToolArgumentValue(item)
			if !ok {
				return "", false
			}
			values = append(values, formatted)
		}
		return "[" + strings.Join(values, ", ") + "]", true
	default:
		return "", false
	}
}

func isSensitiveToolArgument(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	normalized = strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(normalized)
	for _, fragment := range []string{"auth", "bearer", "credential", "password", "passwd", "passphrase", "secret", "token", "cookie", "header", "key", "jwt", "session"} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

func sanitizeDiscordInline(value string) string {
	value = sanitizeDiscordDisplayText(value, false)
	return strings.ReplaceAll(value, "`", "'")
}

func sanitizeDiscordDisplayText(value string, preserveNewlines bool) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' && preserveNewlines {
			return r
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	if !preserveNewlines {
		value = strings.Join(strings.Fields(value), " ")
	}
	value = strings.ReplaceAll(value, "@", "@\u200b")
	return strings.TrimSpace(value)
}

func truncateDiscordText(value string, limit int) string {
	if discordContentLength(value) <= limit {
		return value
	}
	if limit <= 3 {
		return ""
	}
	remaining := limit - 3
	var result strings.Builder
	for _, r := range value {
		units := 1
		if r > 0xffff {
			units = 2
		}
		if units > remaining {
			break
		}
		result.WriteRune(r)
		remaining -= units
	}
	return result.String() + "..."
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
