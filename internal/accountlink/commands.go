package accountlink

import (
	"fmt"
	"strconv"
	"strings"
)

// CommandHandler manages the shared /connect and /disconnect command flow.
// Commands are stateless so they never overlap with ordinary prompts.
type CommandHandler struct {
	links *Service
}

// NewCommandHandler creates a command handler backed by the shared account-link service.
func NewCommandHandler(links *Service) *CommandHandler {
	return &CommandHandler{links: links}
}

// Handle processes account-link commands for a canonical user.
func (h *CommandHandler) Handle(canonicalUserID, input string) (string, bool, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", false, nil
	}
	if !strings.HasPrefix(trimmed, "/") {
		return "", false, nil
	}

	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "", false, nil
	}

	switch fields[0] {
	case "/connect":
		return h.handleConnect(canonicalUserID, fields)
	case "/disconnect":
		return h.handleDisconnect(canonicalUserID, fields)
	default:
		return "", false, nil
	}
}

func (h *CommandHandler) handleConnect(canonicalUserID string, fields []string) (string, bool, error) {
	if len(fields) == 1 {
		return h.startConnect(canonicalUserID)
	}

	selection, err := strconv.Atoi(fields[1])
	if err != nil {
		return connectUsage(), true, nil
	}
	option, ok := GatewayOptionByIndex(selection)
	if !ok {
		return "That gateway number is not valid.\n\n" + connectUsage(), true, nil
	}
	if len(fields) < 3 {
		return connectUsageForOption(option), true, nil
	}

	identifier := strings.TrimSpace(strings.Join(fields[2:], " "))
	if identifier == "" {
		return connectUsageForOption(option), true, nil
	}

	accounts, err := h.links.AccountsForUser(canonicalUserID)
	if err != nil {
		return "", true, err
	}
	for _, account := range accounts {
		if account.Gateway == option.Key {
			return fmt.Sprintf("%s is already connected as %s. Use /disconnect first if you want to replace it.", option.Label, account.Identifier), true, nil
		}
	}

	result, err := h.links.LinkAccount(canonicalUserID, option.Key, identifier, "")
	if err != nil {
		return fmt.Sprintf("Could not link that %s account: %v", option.Label, err), true, nil
	}

	updatedAccounts, err := h.links.AccountsForUser(result.CanonicalUserID)
	if err != nil {
		return "", true, err
	}

	message := fmt.Sprintf("Linked %s as %s.", option.Label, result.LinkedAccount.Identifier)
	if result.AlreadyLinked {
		message = fmt.Sprintf("%s is already linked as %s.", option.Label, result.LinkedAccount.Identifier)
	}
	if result.Merged {
		message += fmt.Sprintf(" Existing memories were merged into %s.", result.CanonicalUserID)
	}
	message += "\n\nLinked accounts:\n" + renderLinkedAccounts(updatedAccounts)
	return message, true, nil
}

func (h *CommandHandler) handleDisconnect(canonicalUserID string, fields []string) (string, bool, error) {
	if len(fields) == 1 {
		return h.startDisconnect(canonicalUserID)
	}
	if len(fields) != 2 {
		return disconnectUsage(canonicalUserID, h.links), true, nil
	}

	selection, err := strconv.Atoi(fields[1])
	if err != nil {
		return disconnectUsage(canonicalUserID, h.links), true, nil
	}

	accounts, err := h.links.AccountsForUser(canonicalUserID)
	if err != nil {
		return "", true, err
	}
	if selection < 1 || selection > len(accounts) {
		return disconnectUsage(canonicalUserID, h.links), true, nil
	}

	account := accounts[selection-1]
	if err := h.links.DisconnectAccount(canonicalUserID, account.Gateway, account.Identifier); err != nil {
		return fmt.Sprintf("Could not disconnect %s: %v", gatewayLabel(account.Gateway), err), true, nil
	}

	remaining, err := h.links.AccountsForUser(canonicalUserID)
	if err != nil {
		return "", true, err
	}
	message := fmt.Sprintf("Disconnected %s: %s.\n\nRemaining linked accounts:\n%s", gatewayLabel(account.Gateway), account.Identifier, renderLinkedAccounts(remaining))
	return message, true, nil
}

func (h *CommandHandler) startConnect(canonicalUserID string) (string, bool, error) {
	accounts, err := h.links.AccountsForUser(canonicalUserID)
	if err != nil {
		return "", true, err
	}
	connected := make(map[string]bool, len(accounts))
	for _, account := range accounts {
		connected[account.Gateway] = true
	}

	var lines []string
	lines = append(lines, "Connect an account.")
	for i, option := range SupportedGateways {
		status := ""
		if connected[option.Key] {
			status = " (connected)"
		}
		lines = append(lines, fmt.Sprintf("%d. %s%s", i+1, option.Label, status))
	}
	lines = append(lines, "")
	lines = append(lines, "Use: /connect <number> <identifier>")
	lines = append(lines, "Example: /connect 1 123456789012345678")

	return strings.Join(lines, "\n"), true, nil
}

func (h *CommandHandler) startDisconnect(canonicalUserID string) (string, bool, error) {
	accounts, err := h.links.AccountsForUser(canonicalUserID)
	if err != nil {
		return "", true, err
	}
	if len(accounts) <= 1 {
		return "You only have one linked account. Oswald will not disconnect the last account.", true, nil
	}

	var lines []string
	lines = append(lines, "Disconnect an account.")
	for i, account := range accounts {
		lines = append(lines, fmt.Sprintf("%d. %s: %s", i+1, gatewayLabel(account.Gateway), account.Identifier))
	}
	lines = append(lines, "")
	lines = append(lines, "Use: /disconnect <number>")
	lines = append(lines, "The last linked account cannot be removed.")

	return strings.Join(lines, "\n"), true, nil
}

func gatewayLabel(key string) string {
	if option, ok := GatewayOptionByKey(key); ok {
		return option.Label
	}
	return key
}

func renderLinkedAccounts(accounts []LinkedAccount) string {
	if len(accounts) == 0 {
		return "- none"
	}
	lines := make([]string, 0, len(accounts))
	for _, account := range accounts {
		status := "unverified"
		if account.Verified {
			status = "verified"
		}
		lines = append(lines, fmt.Sprintf("- %s: %s (%s)", gatewayLabel(account.Gateway), account.Identifier, status))
	}
	return strings.Join(lines, "\n")
}

func connectUsage() string {
	return "Use /connect to list gateways, then /connect <number> <identifier> to link one."
}

func connectUsageForOption(option GatewayOption) string {
	message := fmt.Sprintf("Use: /connect %d <%s>", gatewayIndex(option.Key), strings.ToLower(option.Label)+"-identifier")
	if option.IdentifierExample != "" {
		message += "\nExample: /connect " + strconv.Itoa(gatewayIndex(option.Key)) + " " + option.IdentifierExample
	}
	return message
}

func disconnectUsage(canonicalUserID string, links *Service) string {
	accounts, err := links.AccountsForUser(canonicalUserID)
	if err != nil || len(accounts) == 0 {
		return "Use /disconnect to list linked accounts, then /disconnect <number> to remove one."
	}
	var lines []string
	lines = append(lines, "Use /disconnect <number> to remove a linked account:")
	for i, account := range accounts {
		lines = append(lines, fmt.Sprintf("%d. %s: %s", i+1, gatewayLabel(account.Gateway), account.Identifier))
	}
	return strings.Join(lines, "\n")
}

func gatewayIndex(key string) int {
	for i, option := range SupportedGateways {
		if option.Key == key {
			return i + 1
		}
	}
	return 0
}
