package media

import (
	"fmt"
	"strings"
)

// AttachmentLabel returns a readable label for an attachment in user-facing prompt notes.
func AttachmentLabel(name, mimeType string) string {
	name = strings.TrimSpace(name)
	if name != "" {
		return name
	}
	mimeType = strings.TrimSpace(mimeType)
	if mimeType != "" {
		return mimeType
	}
	return "unknown file"
}

// AugmentPromptWithUnsupportedFiles appends a short note about unusable attachments.
func AugmentPromptWithUnsupportedFiles(prompt string, unsupported []string) string {
	unsupported = compactUnsupportedLabels(unsupported)
	if len(unsupported) == 0 {
		return strings.TrimSpace(prompt)
	}

	note := unsupportedFilesNote(unsupported)
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return note
	}
	return prompt + "\n\n" + note
}

func unsupportedFilesNote(unsupported []string) string {
	if len(unsupported) == 1 {
		return fmt.Sprintf("[User attached an unsupported file: %s]", unsupported[0])
	}
	return fmt.Sprintf("[User attached unsupported files: %s]", strings.Join(unsupported, ", "))
}

func compactUnsupportedLabels(labels []string) []string {
	seen := make(map[string]struct{}, len(labels))
	result := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		result = append(result, label)
	}
	return result
}
