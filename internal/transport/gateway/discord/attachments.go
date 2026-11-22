package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/app/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
)

func (dg *Gateway) loadImages(attachments []Attachment, scoped ...*config.Logger) ([]llm.InputImage, []string) {
	return dg.loadImagesLimit(attachments, media.MaxImagesPerRequest, scoped...)
}

func (dg *Gateway) loadImagesLimit(attachments []Attachment, maxImages int, scoped ...*config.Logger) ([]llm.InputImage, []string) {
	log := dg.log(scoped...)
	if len(attachments) == 0 {
		return nil, nil
	}
	if maxImages <= 0 {
		return nil, unsupportedImageLabels(attachments)
	}

	images := make([]llm.InputImage, 0, len(attachments))
	unsupported := make([]string, 0)
	for _, attachment := range attachments {
		label := media.AttachmentLabel(attachment.Filename, attachment.ContentType)
		if media.LooksLikeMediaMIME(attachment.ContentType) || media.IsMediaExtension(attachment.Filename) {
			continue // GIF/video attachments stay with the media lane.
		}
		if len(images) >= maxImages {
			if isDocumentCandidate(attachment.Filename, attachment.ContentType) {
				continue
			}
			unsupported = append(unsupported, label)
			continue
		}
		if attachment.ContentType != "" && !media.LooksLikeImageMIME(attachment.ContentType) {
			if isDocumentCandidate(attachment.Filename, attachment.ContentType) {
				continue
			}
			unsupported = append(unsupported, label)
			continue
		}
		if attachment.Size > media.MaxImageBytes {
			unsupported = append(unsupported, label)
			continue
		}

		image, err := dg.fetchAttachmentImage(attachment.ID, attachment.URL, attachment.ContentType, attachment.Filename, log)
		if err != nil {
			log.Debug("gateway.attachment.rejected", "rejected discord attachment", config.F("status", "degraded"))
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

func isDocumentCandidate(filename, contentType string) bool {
	if media.IsExtractableDocument(filename) {
		return true
	}
	mime := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	return false
}

// loadDocuments downloads supported document attachments into the private
// document cache. Image payloads stay with the image lane; anything outside
// the extractable set is reported as unsupported.
func (dg *Gateway) loadDocuments(attachments []Attachment, userID string, scoped ...*config.Logger) ([]media.DocumentRef, []string) {
	log := dg.log(scoped...)
	if len(attachments) == 0 {
		return nil, nil
	}
	docs := make([]media.DocumentRef, 0, len(attachments))
	unsupported := make([]string, 0)
	total := 0
	for _, attachment := range attachments {
		label := media.AttachmentLabel(attachment.Filename, attachment.ContentType)
		if media.LooksLikeImageMIME(attachment.ContentType) {
			continue
		}
		if !isDocumentCandidate(attachment.Filename, attachment.ContentType) {
			continue
		}
		if len(docs) >= media.MaxDocumentsPerRequest {
			unsupported = append(unsupported, label)
			continue
		}
		if attachment.Size > media.MaxDocumentBytes {
			unsupported = append(unsupported, label)
			continue
		}
		if total+attachment.Size > media.MaxTotalDocumentBytes {
			unsupported = append(unsupported, label)
			continue
		}
		if dg.DocCache == nil {
			unsupported = append(unsupported, label)
			continue
		}
		data, err := dg.fetchDocumentBytes(attachment.URL, label)
		if err != nil {
			log.Debug("gateway.document.rejected", "rejected discord document", config.F("status", "degraded"))
			unsupported = append(unsupported, label)
			continue
		}
		total += len(data)
		if total > media.MaxTotalDocumentBytes {
			unsupported = append(unsupported, label)
			continue
		}
		name, mime, err := media.SniffDocument(attachment.Filename, attachment.ContentType, data)
		if err != nil {
			log.Debug("gateway.document.rejected", "rejected discord document", config.F("status", "degraded"))
			unsupported = append(unsupported, label)
			continue
		}
		path, err := dg.DocCache.Save(context.Background(), userID, name, data)
		if err != nil {
			log.Debug("gateway.document.rejected", "rejected discord document", config.F("status", "degraded"))
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

func (dg *Gateway) fetchDocumentBytes(rawURL, label string) ([]byte, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("document URL is empty")
	}
	resp, err := dg.httpClient(15 * time.Second).Get(rawURL)
	if err != nil {
		return nil, fmt.Errorf("download document: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("download document: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read document: %w", err)
	}
	if len(body) > media.MaxDocumentBytes {
		return nil, fmt.Errorf("document exceeds %d bytes", media.MaxDocumentBytes)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("document is empty")
	}
	return body, nil
}

func discordAttachmentLabels(attachments []Attachment) []string {
	labels := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		labels = append(labels, media.AttachmentLabel(attachment.Filename, attachment.ContentType))
	}
	return labels
}

// unsupportedImageLabels reports non-media labels when the image lane is
// closed; GIF/video attachments belong to the media lane and are never
// downgraded from here.
func unsupportedImageLabels(attachments []Attachment) []string {
	labels := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		if media.LooksLikeMediaMIME(attachment.ContentType) || media.IsMediaExtension(attachment.Filename) {
			continue
		}
		labels = append(labels, media.AttachmentLabel(attachment.Filename, attachment.ContentType))
	}
	return labels
}

// loadMedia admits every raw inbound file into the private media cache:
// GIF/video/audio for agent ffmpeg inspection, and a catch-all bucket for
// everything the image and document lanes did not claim. Nothing is inlined:
// prompts carry the cache path plus best-effort duration/dimensions.
func (dg *Gateway) loadMedia(attachments []Attachment, userID string, scoped ...*config.Logger) ([]media.DocumentRef, []string) {
	log := dg.log(scoped...)
	if len(attachments) == 0 {
		return nil, nil
	}
	refs := make([]media.DocumentRef, 0, len(attachments))
	unsupported := make([]string, 0)
	total := 0
	for _, attachment := range attachments {
		label := media.AttachmentLabel(attachment.Filename, attachment.ContentType)
		av := media.LooksLikeMediaMIME(attachment.ContentType) || media.IsMediaExtension(attachment.Filename)
		if !av && !mediaCatchAll(attachment.Filename, attachment.ContentType) {
			continue
		}
		if len(refs) >= media.MaxMediaPerRequest {
			unsupported = append(unsupported, label)
			continue
		}
		if attachment.Size > media.MaxMediaBytes {
			unsupported = append(unsupported, label)
			continue
		}
		if total+attachment.Size > media.MaxTotalMediaBytes {
			unsupported = append(unsupported, label)
			continue
		}
		if dg.MediaCache == nil {
			unsupported = append(unsupported, label)
			continue
		}
		data, err := dg.fetchMediaBytes(attachment.URL, label)
		if err != nil {
			log.Debug("gateway.media.rejected", "rejected discord media", config.F("status", "degraded"))
			unsupported = append(unsupported, label)
			continue
		}
		total += len(data)
		if total > media.MaxTotalMediaBytes {
			unsupported = append(unsupported, label)
			continue
		}
		lane := media.LaneAttachment
		var name, mime string
		if av {
			lane = media.LaneMedia
			name, mime, err = media.SniffMedia(attachment.Filename, attachment.ContentType, data)
		} else {
			name, mime, err = media.SniffAttachment(attachment.Filename, attachment.ContentType, data)
		}
		if err != nil {
			log.Debug("gateway.media.rejected", "rejected discord media", config.F("status", "degraded"))
			unsupported = append(unsupported, label)
			continue
		}
		path, err := dg.MediaCache.Save(context.Background(), userID, name, data)
		if err != nil {
			log.Debug("gateway.media.rejected", "rejected discord media", config.F("status", "degraded"))
			unsupported = append(unsupported, label)
			continue
		}
		ref := media.DocumentRef{Path: path, Filename: name, MIMEType: mime, Size: int64(len(data)), Lane: lane}
		if av {
			ref.DurationSeconds, ref.Width, ref.Height = media.ProbeMedia(context.Background(), "", path)
		}
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return nil, unsupported
	}
	return refs, unsupported
}

// mediaCatchAll reports whether an attachment belongs to the catch-all raw
// bucket: not claimed by the image lane (declared image MIME or an image
// extension hint) and not an extractable document.
func mediaCatchAll(filename, contentType string) bool {
	if contentType != "" {
		if media.LooksLikeImageMIME(contentType) {
			return false
		}
	} else if media.LooksLikeImageExtension(filename) {
		return false
	}
	return !isDocumentCandidate(filename, contentType)
}

func (dg *Gateway) fetchMediaBytes(rawURL, label string) ([]byte, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("media URL is empty")
	}
	resp, err := dg.httpClient(15 * time.Second).Get(rawURL)
	if err != nil {
		return nil, fmt.Errorf("download media: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("download media: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxMediaBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read media: %w", err)
	}
	if len(body) > media.MaxMediaBytes {
		return nil, fmt.Errorf("media %q exceeds %d bytes", label, media.MaxMediaBytes)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("media is empty")
	}
	return body, nil
}

func (dg *Gateway) loadEmbedImagesLimit(embeds []Embed, maxImages int, userID string, scoped ...*config.Logger) ([]llm.InputImage, []media.DocumentRef, []string) {
	log := dg.log(scoped...)
	if len(embeds) == 0 {
		return nil, nil, nil
	}

	images := make([]llm.InputImage, 0, len(embeds))
	mediaRefs := make([]media.DocumentRef, 0)
	unsupported := make([]string, 0)
	for _, embed := range embeds {
		if !discordEmbedIsIntentionalMedia(embed) {
			continue
		}
		label := discordEmbedLabel(embed)
		if videoURL := discordEmbedVideoURL(embed); videoURL != "" {
			if len(mediaRefs) >= media.MaxMediaPerRequest {
				unsupported = append(unsupported, label)
				continue
			}
			ref, err := dg.loadEmbedMedia(videoURL, label, userID)
			if err == nil {
				mediaRefs = append(mediaRefs, ref)
				continue
			}
			log.Warn("gateway.embed.video_fallback", "failed to cache animated discord embed; using static preview",
				config.F("status", "degraded"), config.ErrorField(err))
		}

		if len(images) >= maxImages {
			unsupported = append(unsupported, label)
			continue
		}
		assetURL := discordEmbedImageURL(embed)
		if assetURL == "" {
			continue
		}
		image, err := dg.fetchAttachmentImage("", assetURL, "", label, log)
		if err != nil {
			log.Debug("gateway.embed.rejected", "rejected discord embed image", config.F("status", "degraded"), config.ErrorField(err))
			unsupported = append(unsupported, label)
			continue
		}
		if image.Data == "" {
			unsupported = append(unsupported, label)
			continue
		}
		images = append(images, image)
	}

	if len(images) == 0 && len(mediaRefs) == 0 {
		return nil, nil, unsupported
	}
	return images, mediaRefs, unsupported
}

func discordEmbedLabels(embeds []Embed) []string {
	labels := make([]string, 0, len(embeds))
	for _, embed := range embeds {
		if !discordEmbedIsIntentionalMedia(embed) {
			continue
		}
		if discordEmbedVideoURL(embed) != "" || discordEmbedImageURL(embed) != "" {
			labels = append(labels, discordEmbedLabel(embed))
		}
	}
	return labels
}

func discordEmbedIsIntentionalMedia(embed Embed) bool {
	switch strings.ToLower(strings.TrimSpace(embed.Type)) {
	case "image", "gifv":
		return true
	default:
		return false
	}
}

func discordEmbedLabel(embed Embed) string {
	embedType := strings.TrimSpace(embed.Type)
	if embedType == "" {
		embedType = "link"
	}
	return media.AttachmentLabel("discord embed", embedType)
}

func discordEmbedImageURL(embed Embed) string {
	if url := strings.TrimSpace(embed.Image.ProxyURL); url != "" {
		return url
	}
	if url := strings.TrimSpace(embed.Image.URL); url != "" {
		return url
	}
	if url := strings.TrimSpace(embed.Thumbnail.ProxyURL); url != "" {
		return url
	}
	return strings.TrimSpace(embed.Thumbnail.URL)
}

func discordEmbedVideoURL(embed Embed) string {
	if !strings.EqualFold(strings.TrimSpace(embed.Type), "gifv") {
		return ""
	}
	if url := strings.TrimSpace(embed.Video.ProxyURL); url != "" {
		return url
	}
	return strings.TrimSpace(embed.Video.URL)
}

func stripEmbedURLsFromText(text string, embeds []Embed) string {
	for _, rawURL := range discordEmbedSourceURLs(embeds) {
		text = strings.ReplaceAll(text, rawURL, "")
	}
	return strings.Join(strings.Fields(text), " ")
}

func discordEmbedSourceURLs(embeds []Embed) []string {
	urls := make([]string, 0, len(embeds)*7)
	seen := make(map[string]struct{}, len(embeds)*7)
	for _, embed := range embeds {
		if !discordEmbedIsIntentionalMedia(embed) {
			continue
		}
		for _, rawURL := range []string{
			embed.URL,
			embed.Image.URL,
			embed.Image.ProxyURL,
			embed.Thumbnail.URL,
			embed.Thumbnail.ProxyURL,
			embed.Video.URL,
			embed.Video.ProxyURL,
		} {
			rawURL = strings.TrimSpace(rawURL)
			if rawURL == "" {
				continue
			}
			if _, ok := seen[rawURL]; ok {
				continue
			}
			seen[rawURL] = struct{}{}
			urls = append(urls, rawURL)
		}
	}
	return urls
}

func (dg *Gateway) loadEmbedMedia(rawURL, label, userID string) (media.DocumentRef, error) {
	if dg.MediaCache == nil {
		return media.DocumentRef{}, fmt.Errorf("media cache unavailable")
	}
	data, err := dg.fetchMediaBytes(rawURL, label)
	if err != nil {
		return media.DocumentRef{}, err
	}
	name := "discord-embed" + embedMediaExtension(rawURL)
	mime := "video/mp4"
	switch strings.ToLower(filepath.Ext(name)) {
	case ".gif":
		mime = "image/gif"
	case ".webm":
		mime = "video/webm"
	case ".mov":
		mime = "video/quicktime"
	case ".mkv":
		mime = "video/x-matroska"
	}
	name, mime, err = media.SniffMedia(name, mime, data)
	if err != nil {
		return media.DocumentRef{}, err
	}
	path, err := dg.MediaCache.Save(context.Background(), userID, name, data)
	if err != nil {
		return media.DocumentRef{}, err
	}
	duration, width, height := media.ProbeMedia(context.Background(), "", path)
	return media.DocumentRef{Path: path, Filename: name, MIMEType: mime, Size: int64(len(data)), Lane: media.LaneMedia, DurationSeconds: duration, Width: width, Height: height}, nil
}

func embedMediaExtension(rawURL string) string {
	path := rawURL
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	ext := strings.ToLower(filepath.Ext(path))
	if media.IsMediaExtension("media" + ext) {
		return ext
	}
	return ".mp4"
}

func (dg *Gateway) fetchAttachmentImage(attachmentID, rawURL, declaredMIME, filename string, scoped ...*config.Logger) (llm.InputImage, error) {
	log := dg.log(scoped...)
	if strings.TrimSpace(rawURL) == "" {
		return llm.InputImage{}, nil
	}

	resp, err := dg.httpClient(15 * time.Second).Get(rawURL)
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("download attachment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Debug("gateway.attachment.fetch_failed", "failed to fetch discord attachment", config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
		return llm.InputImage{}, fmt.Errorf("download attachment: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxImageBytes+1))
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("read attachment: %w", err)
	}
	if len(body) > media.MaxImageBytes {
		return llm.InputImage{}, fmt.Errorf("attachment exceeds %d bytes", media.MaxImageBytes)
	}

	result, err := media.NormalizeInputImageFromBytes(resp.Header, declaredMIME, body, filename)
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("attachment rejected: %w", err)
	}
	log.Debug("gateway.attachment.normalized", "normalized discord attachment", config.F("normalized_mime", result.Image.MimeType), config.F("attachment_bytes", len(body)), config.F("original_width", result.OriginalWidth), config.F("original_height", result.OriginalHeight), config.F("width", result.Width), config.F("height", result.Height), config.F("is_resized", result.WasResized), config.F("normalized_bytes", result.NormalizedBytes), config.F("base64_chars", result.Base64Chars), config.F("preserved_alpha", result.PreservedAlpha), config.F("used_declared_mime", result.UsedDeclaredMIME))
	return result.Image, nil
}

// sendCommandAttachment posts ordered in-memory command attachments to Discord.
func (dg *Gateway) sendCommandAttachment(channelID string, result commands.Result, replyToID string) (string, error) {
	return dg.sendAttachmentNonce(channelID, result, replyToID, "")
}

func (dg *Gateway) sendAttachmentNonce(channelID string, result commands.Result, replyToID, nonce string) (string, error) {
	if err := result.ValidateAttachments(); err != nil {
		return "", err
	}
	attachments := result.Attachments
	if len(attachments) == 0 {
		return dg.sendMessage(channelID, result.Text, replyToID)
	}

	metadata := make([]map[string]any, 0, len(attachments))
	for i, attachment := range attachments {
		metadata = append(metadata, map[string]any{"id": i, "filename": attachment.Filename})
	}
	payload := map[string]any{
		"content":     result.Text,
		"attachments": metadata,
	}
	if nonce != "" {
		payload["nonce"], payload["enforce_nonce"] = nonce, true
	}
	if replyToID != "" {
		payload["message_reference"] = map[string]string{"message_id": replyToID}
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal Discord attachment payload: %w", err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("payload_json", string(payloadJSON)); err != nil {
		return "", fmt.Errorf("write Discord attachment payload: %w", err)
	}
	for i, attachment := range attachments {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": fmt.Sprintf("files[%d]", i), "filename": attachment.Filename}))
		header.Set("Content-Type", attachment.MIMEType)
		part, err := writer.CreatePart(header)
		if err != nil {
			return "", fmt.Errorf("create Discord attachment part %d: %w", i, err)
		}
		if _, err := part.Write(attachment.Data); err != nil {
			return "", fmt.Errorf("write Discord attachment %d: %w", i, err)
		}
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close Discord attachment payload: %w", err)
	}

	endpoint := fmt.Sprintf("%s/channels/%s/messages", dg.apiBaseURL(), channelID)
	req, err := http.NewRequestWithContext(dg.restContext(), http.MethodPost, endpoint, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bot "+dg.Token)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := dg.httpClient(15 * time.Second).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return "", discordRateLimitError{retryAfter: discordRetryAfter(resp, body)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", discordHTTPError(resp.StatusCode)
	}
	var created createMessageResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&created); err != nil {
		return "", err
	}
	if nonce != "" && created.ID == "" {
		return "", io.ErrUnexpectedEOF
	}
	return created.ID, nil
}

// dropAttachedLabels removes labels for files saved by another lane, so one
// file is never both attached and reported unreadable. Labels are matched
// with the same AttachmentLabel rendering both lanes use.
func dropAttachedLabels(labels []string, refs []media.DocumentRef) []string {
	if len(labels) == 0 || len(refs) == 0 {
		return labels
	}
	attached := make(map[string]int, len(refs))
	for _, ref := range refs {
		attached[media.AttachmentLabel(ref.Filename, ref.MIMEType)]++
	}
	kept := make([]string, 0, len(labels))
	for _, label := range labels {
		if attached[label] > 0 {
			attached[label]--
			continue
		}
		kept = append(kept, label)
	}
	return kept
}

// compactAttachmentLabels keeps one label per file: lanes that all declined
// the same attachment never inflate the reported count.
func compactAttachmentLabels(labels []string) []string {
	seen := make(map[string]struct{}, len(labels))
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		out = append(out, label)
	}
	return out
}
