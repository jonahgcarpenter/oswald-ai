package accountlinking

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
)

type handler struct {
	definition commands.Definition
	links      *Service
}

// New creates account-link command handlers backed by the shared account-link service.
func New(links *Service) []commands.Handler {
	return []commands.Handler{
		&handler{links: links, definition: commands.Definition{Name: "connect", Summary: "Link another gateway account.", Usage: "/connect [gateway_number identifier]"}},
		&handler{links: links, definition: commands.Definition{Name: "disconnect", Summary: "Disconnect a linked gateway account.", Usage: "/disconnect [account_number]"}},
	}
}

// Definition describes the command handled by h.
func (h *handler) Definition() commands.Definition {
	return h.definition
}

// Execute processes one account-link command.
func (h *handler) Execute(_ context.Context, req commands.Request) (commands.Result, error) {
	switch req.Name {
	case "connect":
		return h.handleConnect(req.Principal.CanonicalUserID, req.Args)
	case "disconnect":
		return h.handleDisconnect(req.Principal.CanonicalUserID, req.Args)
	default:
		return commands.Result{Text: "Unknown command: /" + req.Name}, nil
	}
}

func (h *handler) handleConnect(canonicalUserID string, args []string) (commands.Result, error) {
	if len(args) == 0 {
		return h.startConnect(canonicalUserID)
	}

	selection, err := strconv.Atoi(args[0])
	if err != nil {
		return commands.Result{Text: commands.UsageText(h.definition)}, nil
	}
	option, ok := GatewayOptionByIndex(selection)
	if !ok {
		return commands.Result{Text: "That gateway number is not valid.\n\n" + commands.UsageText(h.definition)}, nil
	}
	if len(args) < 2 {
		return commands.Result{Text: connectUsageForOption(h.definition, option)}, nil
	}

	identifier := strings.TrimSpace(strings.Join(args[1:], " "))
	if identifier == "" {
		return commands.Result{Text: connectUsageForOption(h.definition, option)}, nil
	}

	accounts, err := h.links.AccountsForUser(canonicalUserID)
	if err != nil {
		return commands.Result{}, err
	}
	for _, account := range accounts {
		if account.Gateway == option.Key {
			return commands.Result{Text: fmt.Sprintf("%s is already connected as %s. Use /disconnect first if you want to replace it.", option.Label, account.Identifier)}, nil
		}
	}

	result, err := h.links.LinkAccount(canonicalUserID, option.Key, identifier, "")
	if err != nil {
		return commands.Result{Text: fmt.Sprintf("Could not link that %s account: %v", option.Label, err)}, nil
	}

	updatedAccounts, err := h.links.AccountsForUser(result.CanonicalUserID)
	if err != nil {
		return commands.Result{}, err
	}

	message := fmt.Sprintf("Linked %s as %s.", option.Label, result.LinkedAccount.Identifier)
	if result.AlreadyLinked {
		message = fmt.Sprintf("%s is already linked as %s.", option.Label, result.LinkedAccount.Identifier)
	}
	if result.Merged {
		message += fmt.Sprintf(" Existing memories were merged into %s.", result.CanonicalUserID)
	}
	message += "\n\nLinked accounts:\n" + renderLinkedAccounts(updatedAccounts)
	return commands.Result{Text: message}, nil
}

func (h *handler) handleDisconnect(canonicalUserID string, args []string) (commands.Result, error) {
	if len(args) == 0 {
		return h.startDisconnect(canonicalUserID)
	}
	if len(args) != 1 {
		return commands.Result{Text: disconnectUsage(h.definition, canonicalUserID, h.links)}, nil
	}

	selection, err := strconv.Atoi(args[0])
	if err != nil {
		return commands.Result{Text: disconnectUsage(h.definition, canonicalUserID, h.links)}, nil
	}

	accounts, err := h.links.AccountsForUser(canonicalUserID)
	if err != nil {
		return commands.Result{}, err
	}
	if selection < 1 || selection > len(accounts) {
		return commands.Result{Text: disconnectUsage(h.definition, canonicalUserID, h.links)}, nil
	}

	account := accounts[selection-1]
	if err := h.links.DisconnectAccount(canonicalUserID, account.Gateway, account.Identifier); err != nil {
		return commands.Result{Text: fmt.Sprintf("Could not disconnect %s: %v", gatewayLabel(account.Gateway), err)}, nil
	}

	remaining, err := h.links.AccountsForUser(canonicalUserID)
	if err != nil {
		return commands.Result{}, err
	}
	message := fmt.Sprintf("Disconnected %s: %s.\n\nRemaining linked accounts:\n%s", gatewayLabel(account.Gateway), account.Identifier, renderLinkedAccounts(remaining))
	return commands.Result{Text: message}, nil
}

func (h *handler) startConnect(canonicalUserID string) (commands.Result, error) {
	accounts, err := h.links.AccountsForUser(canonicalUserID)
	if err != nil {
		return commands.Result{}, err
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

	return commands.Result{Text: strings.Join(lines, "\n")}, nil
}

func (h *handler) startDisconnect(canonicalUserID string) (commands.Result, error) {
	accounts, err := h.links.AccountsForUser(canonicalUserID)
	if err != nil {
		return commands.Result{}, err
	}
	if len(accounts) <= 1 {
		return commands.Result{Text: "You only have one linked account. Oswald will not disconnect the last account."}, nil
	}

	var lines []string
	lines = append(lines, "Disconnect an account.")
	for i, account := range accounts {
		lines = append(lines, fmt.Sprintf("%d. %s: %s", i+1, gatewayLabel(account.Gateway), account.Identifier))
	}
	lines = append(lines, "")
	lines = append(lines, "Use: /disconnect <number>")
	lines = append(lines, "The last linked account cannot be removed.")

	return commands.Result{Text: strings.Join(lines, "\n")}, nil
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

func connectUsageForOption(definition commands.Definition, option GatewayOption) string {
	message := definition.Summary + "\n" + fmt.Sprintf("Use: /connect %d <%s>", gatewayIndex(option.Key), strings.ToLower(option.Label)+"-identifier")
	if option.IdentifierExample != "" {
		message += "\nExample: /connect " + strconv.Itoa(gatewayIndex(option.Key)) + " " + option.IdentifierExample
	}
	return message
}

func disconnectUsage(definition commands.Definition, canonicalUserID string, links *Service) string {
	accounts, err := links.AccountsForUser(canonicalUserID)
	if err != nil || len(accounts) == 0 {
		return commands.UsageText(definition)
	}
	lines := []string{definition.Summary, "Use: /disconnect <number>"}
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
