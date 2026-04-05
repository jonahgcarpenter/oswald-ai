package accountlink

import (
	"fmt"
	"regexp"
	"strings"
)

// GatewayOption defines how a gateway is presented in the shared account-link flow.
type GatewayOption struct {
	Key               string
	Label             string
	IdentifierPrompt  string
	IdentifierExample string
}

// SupportedGateways is the shared list of account-link targets exposed by all gateways.
var SupportedGateways = []GatewayOption{
	{
		Key:               "discord",
		Label:             "Discord",
		IdentifierPrompt:  "Enter the Discord user ID to link.",
		IdentifierExample: "123456789012345678",
	},
	{
		Key:               "websocket",
		Label:             "WebSocket",
		IdentifierPrompt:  "Enter the WebSocket user identifier to link.",
		IdentifierExample: "alice-local",
	},
	{
		Key:               "imessage",
		Label:             "iMessage",
		IdentifierPrompt:  "Enter the iMessage phone number to link.",
		IdentifierExample: "+15551234567",
	},
}

var discordMentionRE = regexp.MustCompile(`<@!?(\d+)>`)

// GatewayOptionByIndex returns the 1-based indexed gateway option.
func GatewayOptionByIndex(index int) (GatewayOption, bool) {
	if index < 1 || index > len(SupportedGateways) {
		return GatewayOption{}, false
	}
	return SupportedGateways[index-1], true
}

// GatewayOptionByKey returns the gateway option for key.
func GatewayOptionByKey(key string) (GatewayOption, bool) {
	key = strings.TrimSpace(strings.ToLower(key))
	for _, option := range SupportedGateways {
		if option.Key == key {
			return option, true
		}
	}
	return GatewayOption{}, false
}

// NormalizeIdentifier normalizes a user-supplied account identifier per gateway.
func NormalizeIdentifier(gateway, identifier string) (string, error) {
	gateway = strings.TrimSpace(strings.ToLower(gateway))
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", fmt.Errorf("identifier cannot be empty")
	}

	switch gateway {
	case "discord":
		if match := discordMentionRE.FindStringSubmatch(identifier); len(match) == 2 {
			identifier = match[1]
		}
		identifier = strings.TrimSpace(identifier)
		if identifier == "" || !regexp.MustCompile(`^\d+$`).MatchString(identifier) {
			return "", fmt.Errorf("Discord identifiers must be numeric user IDs")
		}
		return identifier, nil
	case "imessage":
		identifier = strings.ReplaceAll(identifier, " ", "")
		identifier = strings.ReplaceAll(identifier, "-", "")
		identifier = strings.ReplaceAll(identifier, "(", "")
		identifier = strings.ReplaceAll(identifier, ")", "")
		if strings.HasPrefix(identifier, "00") {
			identifier = "+" + strings.TrimPrefix(identifier, "00")
		}
		if strings.HasPrefix(identifier, "+") {
			if !regexp.MustCompile(`^\+[0-9]{7,15}$`).MatchString(identifier) {
				return "", fmt.Errorf("iMessage phone numbers must look like +15551234567")
			}
			return identifier, nil
		}
		if !regexp.MustCompile(`^[0-9]{7,15}$`).MatchString(identifier) {
			return "", fmt.Errorf("iMessage phone numbers must contain only digits, optionally with a leading +")
		}
		return "+" + identifier, nil
	case "websocket":
		return identifier, nil
	default:
		return "", fmt.Errorf("unsupported gateway %q", gateway)
	}
}

func accountKey(gateway, identifier string) string {
	return strings.ToLower(strings.TrimSpace(gateway)) + ":" + strings.TrimSpace(identifier)
}
