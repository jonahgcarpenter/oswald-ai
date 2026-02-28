package discord

import "strings"

const maxDiscordLength = 1900 // Leave a buffer for formatting/headers

// splitMessage breaks long strings into chunks for Discord
func splitMessage(content string) []string {
	if len(content) <= maxDiscordLength {
		return []string{content}
	}

	var chunks []string
	for len(content) > 0 {
		if len(content) <= maxDiscordLength {
			chunks = append(chunks, content)
			break
		}

		// Find the last newline within the limit to avoid splitting code lines
		splitIdx := strings.LastIndex(content[:maxDiscordLength], "\n")
		if splitIdx == -1 {
			splitIdx = maxDiscordLength
		}

		chunks = append(chunks, content[:splitIdx])
		content = strings.TrimSpace(content[splitIdx:])
	}

	return chunks
}
