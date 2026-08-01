// Package clientauth implements WebSocket client authorization commands.
package clientauth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/runtimeinvalidation"
	"github.com/jonahgcarpenter/oswald-ai/internal/websocketauth"
)

// Service is the WebSocket authorization behavior exposed to commands.
type Service interface {
	ApproveForUser(context.Context, string, string, string) (string, error)
	ApproveNewUser(context.Context, string, string, bool) (string, error)
	ListClients(context.Context, string) ([]websocketauth.Client, error)
	RevokeClient(context.Context, string, string) error
}

// Authorizer resolves administrator status for privileged client approval.
type Authorizer interface {
	IsAdmin(string) (bool, error)
}

type handler struct {
	definition commands.Definition
	service    Service
	auth       Authorizer
}

// New returns the user-level /client handler.
func New(service Service, auth Authorizer) commands.Handler {
	return &handler{service: service, auth: auth, definition: commands.Definition{Name: "client", Summary: "Approve, list, or revoke WebSocket clients.", Usage: "/client approve <code> | approve-new <code> <display_name> | list | revoke <client_id>", UserExclusive: true}}
}

func (h *handler) Definition() commands.Definition { return h.definition }

func (h *handler) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	if h.service == nil {
		return commands.Result{}, fmt.Errorf("websocket client authorization is unavailable")
	}
	if !req.Principal.Authenticated() {
		return commands.Result{Text: "This command requires an authenticated identity."}, nil
	}
	if len(req.Args) == 0 {
		return commands.Result{Text: commands.UsageText(h.definition)}, nil
	}
	switch strings.ToLower(req.Args[0]) {
	case "approve":
		if len(req.Args) != 2 {
			return commands.Result{Text: commands.UsageText(h.definition)}, nil
		}
		_, err := h.service.ApproveForUser(ctx, req.Principal.CanonicalUserID, req.DisplayName, req.Args[1])
		return authResult("Client approved. It can now exchange its device code for tokens.", err)
	case "approve-new":
		if len(req.Args) < 3 {
			return commands.Result{Text: commands.UsageText(h.definition)}, nil
		}
		if h.auth == nil {
			return commands.Result{Text: "You are not allowed to create users."}, nil
		}
		admin, err := h.auth.IsAdmin(req.Principal.CanonicalUserID)
		if err != nil {
			return commands.Result{}, err
		}
		if !admin {
			return commands.Result{Text: "You are not allowed to create users."}, nil
		}
		userID, err := h.service.ApproveNewUser(ctx, req.Args[1], strings.Join(req.Args[2:], " "), false)
		return authResult("Created WebSocket user "+userID+". The client can now exchange its device code for tokens.", err)
	case "list", "clients":
		if len(req.Args) != 1 {
			return commands.Result{Text: commands.UsageText(h.definition)}, nil
		}
		clients, err := h.service.ListClients(ctx, req.Principal.CanonicalUserID)
		if err != nil {
			return commands.Result{}, err
		}
		lines := []string{"WebSocket clients:"}
		for _, client := range clients {
			state := "active"
			if client.RevokedAt != nil {
				state = "revoked"
			}
			lines = append(lines, fmt.Sprintf("%s | %s | %s", client.ClientID, client.ClientName, state))
		}
		if len(lines) == 1 {
			return commands.Result{Text: "No WebSocket clients configured."}, nil
		}
		return commands.Result{Text: strings.Join(lines, "\n")}, nil
	case "revoke":
		if len(req.Args) != 2 {
			return commands.Result{Text: commands.UsageText(h.definition)}, nil
		}
		err := h.service.RevokeClient(ctx, req.Principal.CanonicalUserID, req.Args[1])
		if err != nil {
			return authResult("", err)
		}
		return commands.Result{Text: "WebSocket client revoked.", Invalidation: &runtimeinvalidation.Event{ExternalIdentities: []string{"websocket-client:" + req.Args[1]}, CloseConnections: true}}, nil
	default:
		return commands.Result{Text: commands.UsageText(h.definition)}, nil
	}
}

func authResult(success string, err error) (commands.Result, error) {
	if err == nil {
		return commands.Result{Text: success}, nil
	}
	switch {
	case errors.Is(err, websocketauth.ErrInvalidUserCode), errors.Is(err, websocketauth.ErrExpired):
		return commands.Result{Text: "That device code is invalid or expired."}, nil
	case errors.Is(err, websocketauth.ErrInvalidGrant):
		return commands.Result{Text: "That WebSocket client is unavailable."}, nil
	default:
		return commands.Result{}, err
	}
}
