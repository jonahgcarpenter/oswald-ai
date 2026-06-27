package admin

import (
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
)

const bannedMessage = "You are banned from using Oswald."

// CommandHandler manages admin-only user moderation commands.
type CommandHandler struct {
	users *accountlinking.Service
}

// NewCommandHandler creates an admin command handler backed by canonical users.
func NewCommandHandler(users *accountlinking.Service) *CommandHandler {
	return &CommandHandler{users: users}
}

// BannedMessage returns the canonical response for banned users.
func BannedMessage(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "No reason provided."
	}
	return bannedMessage + "\nReason: " + reason
}

// CanHandle reports whether input is one of the admin commands.
func (h *CommandHandler) CanHandle(input string) bool {
	fields := strings.Fields(strings.TrimSpace(input))
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "/users", "/admin", "/unadmin", "/ban", "/unban":
		return true
	default:
		return false
	}
}

// Handle processes admin commands for a canonical user.
func (h *CommandHandler) Handle(canonicalUserID, input string) (string, bool, error) {
	trimmed := strings.TrimSpace(input)
	if !h.CanHandle(trimmed) {
		return "", false, nil
	}

	isAdmin, err := h.users.IsAdmin(canonicalUserID)
	if err != nil {
		return "", true, err
	}
	if !isAdmin {
		return "You are not allowed to use admin commands.", true, nil
	}

	fields := strings.Fields(trimmed)
	switch fields[0] {
	case "/users":
		return h.handleUsers()
	case "/admin":
		return h.handleSetAdmin(canonicalUserID, fields, true)
	case "/unadmin":
		return h.handleSetAdmin(canonicalUserID, fields, false)
	case "/ban":
		return h.handleBan(canonicalUserID, fields)
	case "/unban":
		return h.handleUnban(canonicalUserID, fields)
	default:
		return "", false, nil
	}
}

func (h *CommandHandler) handleUsers() (string, bool, error) {
	users, err := h.users.ListUsers()
	if err != nil {
		return "", true, err
	}
	if len(users) == 0 {
		return "No users found.", true, nil
	}

	lines := make([]string, 0, len(users)+1)
	lines = append(lines, "Users:")
	for _, user := range users {
		lines = append(lines, fmt.Sprintf("%s | admin=%t | banned=%t | %s | accounts: %s", user.CanonicalUserID, user.IsAdmin, user.IsBanned, user.Intro, renderAccounts(user.Accounts)))
	}
	return strings.Join(lines, "\n"), true, nil
}

func (h *CommandHandler) handleSetAdmin(actorID string, fields []string, isAdmin bool) (string, bool, error) {
	if len(fields) != 2 {
		if isAdmin {
			return "Use: /admin <canonical_id>", true, nil
		}
		return "Use: /unadmin <canonical_id>", true, nil
	}
	targetID := strings.TrimSpace(fields[1])
	if err := h.users.SetAdmin(actorID, targetID, isAdmin); err != nil {
		return fmt.Sprintf("Could not update admin status: %v", err), true, nil
	}
	if isAdmin {
		return fmt.Sprintf("Marked %s as admin.", targetID), true, nil
	}
	return fmt.Sprintf("Removed admin from %s.", targetID), true, nil
}

func (h *CommandHandler) handleBan(actorID string, fields []string) (string, bool, error) {
	if len(fields) < 2 {
		return "Use: /ban <canonical_id> [reason]", true, nil
	}
	targetID := strings.TrimSpace(fields[1])
	reason := ""
	if len(fields) > 2 {
		reason = strings.Join(fields[2:], " ")
	}
	if err := h.users.BanUser(actorID, targetID, reason); err != nil {
		return fmt.Sprintf("Could not ban user: %v", err), true, nil
	}
	return fmt.Sprintf("Banned %s.", targetID), true, nil
}

func (h *CommandHandler) handleUnban(actorID string, fields []string) (string, bool, error) {
	if len(fields) != 2 {
		return "Use: /unban <canonical_id>", true, nil
	}
	targetID := strings.TrimSpace(fields[1])
	if err := h.users.UnbanUser(actorID, targetID); err != nil {
		return fmt.Sprintf("Could not unban user: %v", err), true, nil
	}
	return fmt.Sprintf("Unbanned %s.", targetID), true, nil
}

func renderAccounts(accounts []accountlinking.LinkedAccount) string {
	if len(accounts) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(accounts))
	for _, account := range accounts {
		label := account.Gateway + ":" + account.Identifier
		if account.DisplayName != "" {
			label += " (" + account.DisplayName + ")"
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, ", ")
}
