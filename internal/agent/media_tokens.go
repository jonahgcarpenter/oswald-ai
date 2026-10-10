package agent

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

// mediaTokenPattern matches MEDIA:/absolute/path tokens in final model text.
// Only absolute paths match; relative paths and bare words are left alone.
var mediaTokenPattern = regexp.MustCompile(`MEDIA:(/\S+)`)

// collapseStrippedWhitespace tidies text left behind by removed MEDIA tokens
// without reflowing the model's own paragraphs.
func collapseStrippedWhitespace(content string) string {
	collapsed := regexp.MustCompile(`[ \t]{2,}`).ReplaceAllString(content, " ")
	collapsed = regexp.MustCompile(`\n{3,}`).ReplaceAllString(collapsed, "\n\n")
	return strings.TrimSpace(collapsed)
}

// trimTokenPunctuation strips sentence punctuation a model may append directly
// after a path (for example a trailing period). Managed cache names never end
// in these characters, so trimming cannot alias a different cached file.
func trimTokenPunctuation(path string) string {
	return strings.TrimRight(path, `.,;:!?)]}'"`)
}

// resolveResponseMedia extracts MEDIA:/absolute/path tokens from final model
// text, resolves each against the sender's unexpired managed image cache, and
// returns stripped text plus deliverable attachments. Tokens are delivery
// directives, not message content: they never survive into stored history.
//
// Resolution is confined by imagecache.Resolve to regular, private, unexpired
// cache images owned by senderID; foreign profiles, arbitrary local files,
// expired entries, and non-images fail closed and are stripped text-only. The
// text-only API gateway strips tokens without attaching. Combined tool plus
// media attachments stay within media.ValidateOutputAttachments; overflow and
// filename collisions are dropped.
func (a *Agent) resolveResponseMedia(ctx context.Context, log *config.Logger, gateway, senderID, content string, existing []media.OutputAttachment) (string, []media.OutputAttachment) {
	matches := mediaTokenPattern.FindAllStringSubmatch(content, -1)
	stripped := collapseStrippedWhitespace(mediaTokenPattern.ReplaceAllString(content, ""))
	if len(matches) == 0 || gateway == "openai" || a.imageCache == nil {
		return stripped, nil
	}
	seen := make(map[string]bool, len(matches))
	var attached []media.OutputAttachment
	dropped := 0
	for _, match := range matches {
		path := trimTokenPunctuation(match[1])
		if seen[path] {
			continue
		}
		seen[path] = true
		data, mimeType, err := a.imageCache.Resolve(ctx, senderID, path)
		if err != nil {
			dropped++
			continue
		}
		candidate := media.OutputAttachment{Filename: filepath.Base(path), MIMEType: mimeType, Data: data}
		combined := make([]media.OutputAttachment, 0, len(existing)+len(attached)+1)
		combined = append(combined, existing...)
		combined = append(combined, attached...)
		combined = append(combined, candidate)
		if err := media.ValidateOutputAttachments(combined); err != nil {
			dropped++
			continue
		}
		attached = append(attached, candidate)
	}
	if log != nil {
		log.Debug("agent.response.media", "resolved MEDIA tokens from model response", config.F("attached_count", len(attached)), config.F("dropped_count", dropped))
	}
	return stripped, attached
}
