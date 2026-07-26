// Package memories implements the /memories command surface.
package memories

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	privacyservice "github.com/jonahgcarpenter/oswald-ai/internal/privacy"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

const usage = "/memories list | forget <id|all>"

const maxMemoryListBytes = commands.MaxTotalAttachmentBytes - (utf8.UTFMax-1)*(commands.MaxAttachments-1)

type handler struct{ service *privacyservice.Service }

// New creates the memories command handler.
func New(service *privacyservice.Service) commands.Handler { return handler{service: service} }

func (handler) Definition() commands.Definition {
	return commands.Definition{Name: "memories", Summary: "List or forget your memories.", Usage: usage, UserExclusive: true}
}

func (h handler) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	if h.service == nil {
		return commands.Result{}, fmt.Errorf("memory service is unavailable")
	}
	serviceReq := privacyservice.Request{RequestID: req.RequestID, Principal: req.Principal, SessionKey: req.SessionKey}
	if len(req.Args) == 1 && req.Args[0] == "list" {
		memories, err := h.service.ListMemories(ctx, serviceReq)
		if err != nil {
			return commands.Result{}, err
		}
		return listResult(memories)
	}
	if len(req.Args) != 2 || req.Args[0] != "forget" {
		return commands.Result{Text: commands.UsageText(h.Definition())}, nil
	}
	if strings.EqualFold(req.Args[1], "all") {
		if err := h.service.DeleteAllMemories(ctx, serviceReq); err != nil {
			return commands.Result{}, err
		}
		return h.invalidatingResult(ctx, serviceReq, "All memories and memory source data were forgotten.")
	}
	id, err := usermemory.ParsePrivacyID(req.Args[1])
	if err != nil {
		return commands.Result{Text: "ID must be an exact positive decimal stable ID, or all."}, nil
	}
	if _, err := h.service.ForgetMemory(ctx, serviceReq, id); errors.Is(err, sql.ErrNoRows) {
		return commands.Result{Text: "Memory " + req.Args[1] + " was not found."}, nil
	} else if err != nil {
		return commands.Result{}, err
	}
	return h.invalidatingResult(ctx, serviceReq, "Memory "+req.Args[1]+" was forgotten.")
}

func listResult(memories []usermemory.ListedMemory) (commands.Result, error) {
	var content strings.Builder
	if len(memories) == 0 {
		content.WriteString("No active memories.\n")
	} else {
		for _, memory := range memories {
			fmt.Fprintf(&content, "ID: %d\nCategory: %s\nMemory: %s\n\n", memory.ID, memory.Category, memory.Statement)
		}
	}
	data := []byte(content.String())
	if !utf8.Valid(data) {
		return commands.Result{}, fmt.Errorf("memory list contains invalid UTF-8")
	}
	if len(data) > maxMemoryListBytes {
		return commands.Result{}, fmt.Errorf("memory list exceeds the %d-byte UTF-8 attachment limit", maxMemoryListBytes)
	}
	parts := splitUTF8(data, commands.MaxAttachmentBytes)
	attachments := make([]commands.Attachment, 0, len(parts))
	for i, part := range parts {
		filename := "oswald-memories.txt"
		if len(parts) > 1 {
			filename = fmt.Sprintf("oswald-memories.part%03d.txt", i+1)
		}
		attachments = append(attachments, commands.Attachment{Filename: filename, MIMEType: "text/plain; charset=utf-8", Data: part})
	}
	result := commands.Result{Text: fmt.Sprintf("Your %d active memories are attached.", len(memories)), Attachments: attachments}
	if len(memories) == 1 {
		result.Text = "Your 1 active memory is attached."
	} else if len(memories) == 0 {
		result.Text = "Your memory list is attached."
	}
	if len(attachments) == 1 {
		result.Attachment = &attachments[0]
		result.Attachments = nil
	}
	if err := result.ValidateAttachments(); err != nil {
		return commands.Result{}, fmt.Errorf("memory list cannot be delivered: %w", err)
	}
	return result, nil
}

func splitUTF8(data []byte, limit int) [][]byte {
	parts := make([][]byte, 0, (len(data)+limit-1)/limit)
	for len(data) > limit {
		end := limit
		for end > 0 && !utf8.RuneStart(data[end]) {
			end--
		}
		if end == 0 {
			end = limit
		}
		parts = append(parts, data[:end])
		data = data[end:]
	}
	return append(parts, data)
}

func (h handler) invalidatingResult(ctx context.Context, req privacyservice.Request, text string) (commands.Result, error) {
	event, err := h.service.Invalidation(ctx, req, nil)
	if err != nil {
		return commands.Result{}, err
	}
	return commands.Result{Text: text, Invalidation: &event}, nil
}
