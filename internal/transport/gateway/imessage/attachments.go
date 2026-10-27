package imessage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/app/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
)

func (g *Gateway) loadImages(attachments []attachment, scoped ...*config.Logger) ([]llm.InputImage, []string) {
	return g.loadImagesLimit(attachments, media.MaxImagesPerRequest, scoped...)
}

func (g *Gateway) loadImagesLimit(attachments []attachment, maxImages int, scoped ...*config.Logger) ([]llm.InputImage, []string) {
	log := g.log(scoped...)
	if len(attachments) == 0 {
		return nil, nil
	}
	if maxImages <= 0 {
		return nil, attachmentLabels(attachments)
	}

	images := make([]llm.InputImage, 0, len(attachments))
	unsupported := make([]string, 0)
	for _, attachment := range attachments {
		label := media.AttachmentLabel(attachment.TransferName, attachment.MimeType)
		if len(images) >= maxImages {
			if isDocumentCandidate(attachment.TransferName, attachment.MimeType) {
				continue
			}
			unsupported = append(unsupported, label)
			continue
		}
		if attachment.MimeType != "" && !media.LooksLikeImageMIME(attachment.MimeType) {
			if isDocumentCandidate(attachment.TransferName, attachment.MimeType) {
				continue
			}
			unsupported = append(unsupported, label)
			continue
		}
		if attachment.TotalBytes > media.MaxImageBytes {
			unsupported = append(unsupported, label)
			continue
		}

		image, err := g.fetchAttachmentImage(attachment, log)
		if err != nil {
			log.Debug("gateway.attachment.rejected", "rejected imessage attachment", config.F("status", "degraded"))
			unsupported = append(unsupported, label)
			continue
		}
		if image.Data == "" {
			unsupported = append(unsupported, label)
			continue
		}
		images = append(images, image)
	}

	if len(images) == 0 {
		return nil, unsupported
	}
	return images, unsupported
}

func isDocumentCandidate(filename, mimeType string) bool {
	if media.IsExtractableDocument(filename) {
		return true
	}
	mime := strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	return false
}

func attachmentLabels(attachments []attachment) []string {
	labels := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		labels = append(labels, media.AttachmentLabel(attachment.TransferName, attachment.MimeType))
	}
	return labels
}

func (g *Gateway) fetchAttachmentImage(attachment attachment, scoped ...*config.Logger) (llm.InputImage, error) {
	log := g.log(scoped...)
	if strings.TrimSpace(attachment.GUID) == "" {
		return llm.InputImage{}, nil
	}

	endpoint, err := buildBlueBubblesAttachmentEndpoint(g.BlueBubblesURL, attachment.GUID, g.BlueBubblesPassword)
	if err != nil {
		return llm.InputImage{}, err
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("build BlueBubbles attachment request: %w", err)
	}

	resp, err := g.httpClient().Do(req)
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("download BlueBubbles attachment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Debug("gateway.attachment.fetch_failed", "failed to fetch imessage attachment", config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
		return llm.InputImage{}, fmt.Errorf("download BlueBubbles attachment failed with status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxImageBytes+1))
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("read BlueBubbles attachment: %w", err)
	}
	if len(body) > media.MaxImageBytes {
		return llm.InputImage{}, fmt.Errorf("attachment exceeds %d bytes", media.MaxImageBytes)
	}

	result, err := media.NormalizeInputImageFromBytes(resp.Header, attachment.MimeType, body, attachment.TransferName)
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("attachment rejected: %w", err)
	}
	log.Debug("gateway.attachment.normalized", "normalized imessage attachment", config.F("normalized_mime", result.Image.MimeType), config.F("attachment_bytes", len(body)), config.F("original_width", result.OriginalWidth), config.F("original_height", result.OriginalHeight), config.F("width", result.Width), config.F("height", result.Height), config.F("is_resized", result.WasResized), config.F("normalized_bytes", result.NormalizedBytes), config.F("base64_chars", result.Base64Chars), config.F("preserved_alpha", result.PreservedAlpha), config.F("used_declared_mime", result.UsedDeclaredMIME))

	image := result.Image
	return image, nil
}

// loadDocuments downloads supported document attachments into the private
// document cache. Image payloads stay with the image lane.
func (g *Gateway) loadDocuments(attachments []attachment, userID string, scoped ...*config.Logger) ([]media.DocumentRef, []string) {
	log := g.log(scoped...)
	if len(attachments) == 0 {
		return nil, nil
	}
	docs := make([]media.DocumentRef, 0, len(attachments))
	unsupported := make([]string, 0)
	total := 0
	for _, attachment := range attachments {
		label := media.AttachmentLabel(attachment.TransferName, attachment.MimeType)
		if media.LooksLikeImageMIME(attachment.MimeType) {
			continue
		}
		if !isDocumentCandidate(attachment.TransferName, attachment.MimeType) {
			continue
		}
		if len(docs) >= media.MaxDocumentsPerRequest {
			unsupported = append(unsupported, label)
			continue
		}
		if attachment.TotalBytes > media.MaxDocumentBytes {
			unsupported = append(unsupported, label)
			continue
		}
		if total+attachment.TotalBytes > media.MaxTotalDocumentBytes {
			unsupported = append(unsupported, label)
			continue
		}
		if g.DocCache == nil {
			unsupported = append(unsupported, label)
			continue
		}
		data, err := g.fetchDocumentBytes(attachment)
		if err != nil {
			log.Debug("gateway.document.rejected", "rejected imessage document", config.F("status", "degraded"))
			unsupported = append(unsupported, label)
			continue
		}
		total += len(data)
		if total > media.MaxTotalDocumentBytes {
			unsupported = append(unsupported, label)
			continue
		}
		name, mime, err := media.SniffDocument(attachment.TransferName, attachment.MimeType, data)
		if err != nil {
			log.Debug("gateway.document.rejected", "rejected imessage document", config.F("status", "degraded"))
			unsupported = append(unsupported, label)
			continue
		}
		path, err := g.DocCache.Save(context.Background(), userID, name, data)
		if err != nil {
			log.Debug("gateway.document.rejected", "rejected imessage document", config.F("status", "degraded"))
			unsupported = append(unsupported, label)
			continue
		}
		docs = append(docs, media.DocumentRef{Path: path, Filename: name, MIMEType: mime, Size: int64(len(data))})
	}
	if len(docs) == 0 {
		return nil, unsupported
	}
	return docs, unsupported
}

func (g *Gateway) fetchDocumentBytes(attachment attachment) ([]byte, error) {
	if strings.TrimSpace(attachment.GUID) == "" {
		return nil, fmt.Errorf("attachment GUID is empty")
	}
	endpoint, err := buildBlueBubblesAttachmentEndpoint(g.BlueBubblesURL, attachment.GUID, g.BlueBubblesPassword)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build BlueBubbles attachment request: %w", err)
	}
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("download BlueBubbles attachment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("download BlueBubbles attachment failed with status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read BlueBubbles attachment: %w", err)
	}
	if len(body) > media.MaxDocumentBytes {
		return nil, fmt.Errorf("attachment exceeds %d bytes", media.MaxDocumentBytes)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("attachment is empty")
	}
	return body, nil
}

// sendCommandAttachment sends one in-memory attachment through BlueBubbles.
// When selectedMessageGUID is set and the private API is available the
// attachment is threaded under the user's message, mirroring sendTextReply.
// A failed threaded attempt falls back to a plain top-level send so delivery
// does not hard-fail where threading is unsupported.
func (g *Gateway) sendCommandAttachment(chatGUID, selectedMessageGUID string, attachment commands.Attachment) error {
	if err := attachment.Validate(); err != nil {
		return err
	}
	threadGUID := strings.TrimSpace(selectedMessageGUID)
	if threadGUID != "" && g.blueBubblesPrivateAPIAvailable(g.log()) {
		if err := g.postCommandAttachment(chatGUID, threadGUID, attachment); err == nil {
			return nil
		} else {
			g.log().Warn("gateway.send.retry", "retrying imessage attachment send without private reply fields", config.F("default_method", defaultSendMethod), config.F("status", "retry"), config.ErrorField(err))
		}
	}
	return g.postCommandAttachment(chatGUID, "", attachment)
}

// postCommandAttachment posts one attachment payload. A non-empty
// selectedMessageGUID threads the send via the private API.
func (g *Gateway) postCommandAttachment(chatGUID, selectedMessageGUID string, attachment commands.Attachment) error {
	endpoint, err := buildBlueBubblesEndpoint(g.BlueBubblesURL, "/api/v1/message/attachment", g.BlueBubblesPassword)
	if err != nil {
		return err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fields := map[string]string{
		"chatGuid": chatGUID,
		"name":     attachment.Filename,
		"tempGuid": newTempGUID(),
	}
	if strings.TrimSpace(selectedMessageGUID) != "" {
		fields["selectedMessageGuid"] = strings.TrimSpace(selectedMessageGUID)
		fields["method"] = defaultSendMethod
	}
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			return fmt.Errorf("write BlueBubbles attachment field: %w", err)
		}
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "attachment", "filename": attachment.Filename}))
	header.Set("Content-Type", attachment.MIMEType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("create BlueBubbles attachment part: %w", err)
	}
	if _, err := part.Write(attachment.Data); err != nil {
		return fmt.Errorf("write BlueBubbles attachment: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close BlueBubbles attachment payload: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, &body)
	if err != nil {
		return fmt.Errorf("build BlueBubbles attachment send request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("send BlueBubbles attachment request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("BlueBubbles attachment send failed with status %d", resp.StatusCode)
	}
	return nil
}
