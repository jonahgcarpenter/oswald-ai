package media

import (
	"fmt"
	"mime"
	"strings"
	"unicode"
)

const (
	// MaxOutputAttachmentBytes is the maximum size of one in-memory output attachment.
	MaxOutputAttachmentBytes = 8 << 20
	// MaxOutputAttachments is the maximum number of attachments in one response.
	MaxOutputAttachments = 10
	// MaxTotalOutputAttachmentBytes is the maximum aggregate response attachment size.
	MaxTotalOutputAttachmentBytes = MaxOutputAttachments * MaxOutputAttachmentBytes
)

// OutputAttachment is a transport-neutral in-memory file returned to a gateway.
type OutputAttachment struct {
	Filename string
	MIMEType string
	Data     []byte
}

// Validate checks that the attachment is safe and bounded for transport delivery.
func (a OutputAttachment) Validate() error {
	filename := strings.TrimSpace(a.Filename)
	if filename == "" {
		return fmt.Errorf("attachment filename is required")
	}
	if filename != a.Filename {
		return fmt.Errorf("attachment filename has leading or trailing whitespace")
	}
	if filename == "." || filename == ".." || strings.ContainsAny(filename, `/\\`) {
		return fmt.Errorf("attachment filename must be a base name")
	}
	if len(filename) > 255 {
		return fmt.Errorf("attachment filename exceeds 255 bytes")
	}
	for _, r := range filename {
		if unicode.IsControl(r) {
			return fmt.Errorf("attachment filename contains control characters")
		}
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(a.MIMEType))
	if err != nil || mediaType == "" || !strings.Contains(mediaType, "/") {
		return fmt.Errorf("attachment MIME type is invalid")
	}
	if len(a.Data) == 0 {
		return fmt.Errorf("attachment data is required")
	}
	if len(a.Data) > MaxOutputAttachmentBytes {
		return fmt.Errorf("attachment exceeds %d bytes", MaxOutputAttachmentBytes)
	}
	return nil
}

// ValidateOutputAttachments validates per-file and aggregate transport limits.
func ValidateOutputAttachments(attachments []OutputAttachment) error {
	if len(attachments) > MaxOutputAttachments {
		return fmt.Errorf("response exceeds %d attachments", MaxOutputAttachments)
	}
	filenames := make(map[string]struct{}, len(attachments))
	totalBytes := 0
	for i, attachment := range attachments {
		if err := attachment.Validate(); err != nil {
			return fmt.Errorf("attachment %d: %w", i+1, err)
		}
		if _, exists := filenames[attachment.Filename]; exists {
			return fmt.Errorf("response contains duplicate filename %q", attachment.Filename)
		}
		filenames[attachment.Filename] = struct{}{}
		totalBytes += len(attachment.Data)
		if totalBytes > MaxTotalOutputAttachmentBytes {
			return fmt.Errorf("response attachments exceed %d total bytes", MaxTotalOutputAttachmentBytes)
		}
	}
	return nil
}
