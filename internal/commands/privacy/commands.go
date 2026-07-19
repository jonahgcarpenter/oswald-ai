// Package privacy implements the /privacy command surface.
package privacy

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	privacyservice "github.com/jonahgcarpenter/oswald-ai/internal/privacy"
	"github.com/jonahgcarpenter/oswald-ai/internal/privacyruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

const usage = "/privacy inspect [memories|candidates|sessions|all] [page] | export | forget-memory <id> | delete-memory <id> | delete-candidate <id> | delete-session | delete-all-memories | delete-account | confirm <code>"

type handler struct{ service *privacyservice.Service }

// New creates the privacy command handler.
func New(service *privacyservice.Service) commands.Handler { return handler{service: service} }

func (handler) Definition() commands.Definition {
	return commands.Definition{Name: "privacy", Summary: "Inspect, export, forget, or delete your data.", Usage: usage, UserExclusive: true}
}

func (h handler) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	if h.service == nil {
		return commands.Result{}, fmt.Errorf("privacy service is unavailable")
	}
	if len(req.Args) == 0 {
		return commands.Result{Text: commands.UsageText(h.Definition())}, nil
	}
	serviceReq := privacyservice.Request{RequestID: req.RequestID, Principal: req.Principal, IsDirect: req.IsDirect, SessionKey: req.SessionKey}
	switch req.Args[0] {
	case "inspect":
		if len(req.Args) > 3 {
			return commands.Result{Text: commands.UsageText(h.Definition())}, nil
		}
		section, page := "all", 1
		if len(req.Args) >= 2 {
			section = req.Args[1]
		}
		if len(req.Args) == 3 {
			parsed, err := strconv.Atoi(req.Args[2])
			if err != nil || parsed < 1 {
				return commands.Result{Text: "Page must be a positive integer."}, nil
			}
			page = parsed
		}
		result, err := h.service.Inspect(ctx, serviceReq, section, page)
		if err != nil {
			return commands.Result{}, err
		}
		data, _ := json.MarshalIndent(result, "", "  ")
		return commands.Result{Text: string(data)}, nil
	case "export":
		if len(req.Args) != 1 {
			return commands.Result{Text: commands.UsageText(h.Definition())}, nil
		}
		export, err := h.service.Export(ctx, serviceReq)
		if err != nil {
			return commands.Result{}, err
		}
		return exportResult(export)
	case "forget-memory", "delete-memory", "delete-candidate":
		if len(req.Args) != 2 {
			return commands.Result{Text: commands.UsageText(h.Definition())}, nil
		}
		id, err := usermemory.ParsePrivacyID(req.Args[1])
		if err != nil {
			return commands.Result{Text: "ID must be an exact positive decimal stable ID."}, nil
		}
		switch req.Args[0] {
		case "forget-memory":
			state, err := h.service.ForgetMemory(ctx, serviceReq, id)
			return h.invalidatingResult(ctx, serviceReq, "Memory "+req.Args[1]+" is "+state+".", nil, err)
		case "delete-memory":
			state, err := h.service.DeleteMemory(ctx, serviceReq, id)
			return h.invalidatingResult(ctx, serviceReq, "Memory "+req.Args[1]+" is "+state+".", nil, err)
		default:
			err := h.service.DeleteCandidate(ctx, serviceReq, id)
			return h.invalidatingResult(ctx, serviceReq, "Candidate "+req.Args[1]+" is deleted.", nil, err)
		}
	case "delete-session":
		if len(req.Args) != 1 {
			return commands.Result{Text: commands.UsageText(h.Definition())}, nil
		}
		generation, err := h.service.DeleteSession(ctx, serviceReq)
		if generation == 0 {
			return h.invalidatingResult(ctx, serviceReq, "The current session was already empty.", []string{req.SessionKey}, err)
		}
		return h.invalidatingResult(ctx, serviceReq, fmt.Sprintf("Session generation %d was deleted.", generation), []string{req.SessionKey}, err)
	case "delete-all-memories", "delete-account":
		if len(req.Args) != 1 {
			return commands.Result{Text: commands.UsageText(h.Definition())}, nil
		}
		var challenge privacyservice.Challenge
		var err error
		if req.Args[0] == "delete-account" {
			challenge, err = h.service.BeginDeleteAccount(ctx, serviceReq)
		} else {
			challenge, err = h.service.BeginDeleteAllMemories(ctx, serviceReq)
		}
		if err != nil {
			return commands.Result{}, err
		}
		return commands.Result{Text: "Confirm this operation before it expires with exactly:\n/privacy confirm " + challenge.Code}, nil
	case "confirm":
		if len(req.Args) != 2 || strings.TrimSpace(req.Args[1]) == "" {
			return commands.Result{Text: commands.UsageText(h.Definition())}, nil
		}
		confirmed, err := h.service.Confirm(ctx, serviceReq, req.Args[1])
		if err != nil {
			return commands.Result{}, err
		}
		if confirmed.OperationType == "delete_user" {
			event := privacyruntime.Event{ExternalIdentities: confirmed.ExternalIdentities, SessionIDs: confirmed.SessionIDs, CloseConnections: true}
			return commands.Result{Text: "Your account and retained user data were deleted.", Invalidation: &event}, nil
		}
		event := privacyruntime.Event{ExternalIdentities: confirmed.ExternalIdentities, SessionIDs: confirmed.SessionIDs}
		return commands.Result{Text: "All retained memories and candidates were deleted.", Invalidation: &event}, nil
	default:
		return commands.Result{Text: commands.UsageText(h.Definition())}, nil
	}
}

func exportResult(export privacyservice.Export) (commands.Result, error) {
	attachments := make([]commands.Attachment, 0, len(export.Parts))
	for _, part := range export.Parts {
		attachments = append(attachments, commands.Attachment{Filename: part.Filename, MIMEType: part.MIMEType, Data: part.Data})
	}
	result := commands.Result{Text: "Your privacy export is attached.", Attachments: attachments}
	if len(attachments) > 1 {
		result.Text = fmt.Sprintf("Your privacy export is attached in %d ordered parts. Concatenate the parts byte-for-byte in filename order to reconstruct the exact valid oswald.user-export.v1 JSON file.", len(attachments))
	}
	if err := result.ValidateAttachments(); err != nil {
		return commands.Result{}, fmt.Errorf("privacy export cannot be delivered: %w", err)
	}
	if len(attachments) == 1 {
		result.Attachment = &result.Attachments[0]
		result.Attachments = nil
	}
	return result, nil
}

func (h handler) invalidatingResult(ctx context.Context, req privacyservice.Request, text string, sessionIDs []string, mutationErr error) (commands.Result, error) {
	if mutationErr != nil {
		return commands.Result{}, mutationErr
	}
	event, err := h.service.Invalidation(ctx, req, sessionIDs)
	if err != nil {
		return commands.Result{}, err
	}
	return commands.Result{Text: text, Invalidation: &event}, nil
}
