package websearch

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/webfetch"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

const braveImagesEndpoint = "https://api.search.brave.com/res/v1/images/search"

type imageResultMetadata struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	SourceURL string `json:"source_url"`
	Loaded    bool   `json:"loaded"`
}

// NewImageSearchHandler creates a Brave-only visual search handler. The fixed
// authenticated endpoint and independent protected thumbnail client share no headers.
func NewImageSearchHandler(apiKey string, log *config.Logger) func(context.Context, map[string]interface{}) (governance.Result, error) {
	client := &http.Client{Timeout: braveHTTPTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return newImageSearchHandler(strings.TrimSpace(apiKey), braveImagesEndpoint, client, webfetch.NewClient().DownloadThumbnail, log)
}

func newImageSearchHandler(apiKey, endpoint string, client *http.Client, download func(context.Context, string) (llm.InputImage, error), log *config.Logger) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (result governance.Result, err error) {
		started := time.Now()
		meta := requestctx.MetadataFromContext(ctx)
		parent := meta.OperationID
		if parent == "" {
			parent = meta.ParentOperationID
		}
		meta.OperationID, meta.ParentOperationID = rand.Text(), parent
		ctx = requestctx.WithMetadata(ctx, meta)
		submitted, candidates, loaded, failed := false, 0, 0, 0
		rejected, attemptedDownloads, downloadedBytes := false, 0, 0
		defer func() {
			if log == nil {
				return
			}
			status, outcome := "ok", "ok"
			if loaded == 0 {
				outcome = "empty"
			}
			if result.IsDegraded {
				status, outcome = "degraded", "degraded"
			}
			if err != nil {
				status, outcome = "error", "error"
			}
			if rejected {
				status, outcome = "rejected", "rejected"
			}
			if errors.Is(err, context.Canceled) {
				status, outcome = "ok", "canceled"
			}
			fields := append(requestctx.LogFields(ctx), config.F("record_kind", "measurement"), config.F("provider", "brave"), config.F("operation", "search"), config.F("status", status), config.F("outcome", outcome), config.F("is_submitted", submitted), config.F("duration_ms", time.Since(started).Milliseconds()), config.F("candidate_count", candidates), config.F("image_count", loaded), config.F("failed_count", failed))
			fields = append(fields, config.F("attempted_download_count", attemptedDownloads), config.F("downloaded_image_bytes", downloadedBytes))
			if err != nil {
				fields = append(fields, config.ErrorField(err))
			}
			log.Server("provider.web.image_search").Info("provider.web.image_search.complete", "image search completed", fields...)
		}()
		state := requestctx.ImageSearchStateFromContext(ctx)
		if err = state.ReserveSearch(); err != nil {
			rejected = true
			return result, err
		}
		query, _ := args["query"].(string)
		if err = validateQuery(query); err != nil {
			rejected = true
			return result, err
		}
		if apiKey == "" {
			rejected = true
			return result, errors.New("Brave image search unavailable")
		}
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		if err = ctx.Err(); err != nil {
			return result, err
		}
		u, parseErr := url.Parse(endpoint)
		if parseErr != nil {
			rejected = true
			return result, errors.New("invalid image search endpoint")
		}
		u.RawQuery = url.Values{"q": {strings.TrimSpace(query)}, "count": {"8"}, "safesearch": {"off"}, "country": {"US"}, "search_lang": {"en"}, "spellcheck": {"true"}}.Encode()
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if reqErr != nil {
			rejected = true
			return result, errors.New("invalid image search request")
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Subscription-Token", apiKey)
		submitted = true
		resp, callErr := client.Do(req)
		if callErr != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, errors.New("image search failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return result, &searchHTTPError{status: resp.StatusCode, message: "image search returned unsuccessful status"}
		}
		if resp.ContentLength > maxResponseBytes {
			return result, errors.New("image search response size limit")
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if readErr != nil || len(body) > maxResponseBytes {
			return result, errors.New("image search response size limit")
		}
		var wire struct {
			Type    string `json:"type"`
			Results *[]struct {
				Title     string `json:"title"`
				URL       string `json:"url"`
				Thumbnail struct {
					Src string `json:"src"`
				} `json:"thumbnail"`
			} `json:"results"`
		}
		if json.Unmarshal(body, &wire) != nil || wire.Type != "images" || wire.Results == nil {
			return result, errors.New("invalid image search response")
		}
		refs := make([]requestctx.ImageSearchReference, 0, 2)
		seen := make(map[string]bool)
		candidates = min(len(*wire.Results), 8)
		for _, candidate := range (*wire.Results)[:candidates] {
			if len(refs) == 2 {
				break
			}
			normalized, ok := normalizeResult(searchCandidate{Title: candidate.Title, URL: candidate.URL})
			if !ok || len(normalized.URL) > 1024 || candidate.Thumbnail.Src == "" || seen[candidate.Thumbnail.Src] {
				failed++
				continue
			}
			seen[candidate.Thumbnail.Src] = true
			// Never normalize the proxy URL's query or substitute properties.url.
			attemptedDownloads++
			img, downloadErr := download(ctx, candidate.Thumbnail.Src)
			if downloadErr == nil {
				// The downloader returns standard base64 of normalized image bytes.
				downloadedBytes += base64.StdEncoding.DecodedLen(len(img.Data)) - (len(img.Data) - len(strings.TrimRight(img.Data, "=")))
			}
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			if downloadErr != nil {
				failed++
				continue
			}
			refs = append(refs, requestctx.ImageSearchReference{Title: normalized.Title, SourceURL: normalized.URL, MIMEType: img.MimeType, Data: img.Data})
		}
		refs, err = state.AddReferences(refs)
		if err != nil {
			return result, err
		}
		loaded = len(refs)
		metadata := make([]imageResultMetadata, 0, loaded)
		for _, ref := range refs {
			metadata = append(metadata, imageResultMetadata{ID: ref.ID, Title: ref.Title, SourceURL: ref.SourceURL, Loaded: true})
		}
		encoded, encodeErr := json.Marshal(struct {
			Results []imageResultMetadata `json:"results"`
			Notice  string                `json:"notice"`
		}{metadata, "Untrusted sourced previews, not generated images or identity guarantees. Inspect the injected images in a successful model call before selecting or generating in a subsequent round."})
		if encodeErr != nil || len(encoded) > maxToolResponseBytes {
			return result, errors.New("image search metadata size limit")
		}
		result = governance.Result{Content: string(encoded), Outcome: governance.OutcomeProductive}
		if loaded == 0 {
			result.Outcome = governance.OutcomeUnproductive
			result.ReasonCode = "no_results"
		}
		if failed > 0 {
			result.IsDegraded = true
			result.ReasonCode = "partial_results"
		}
		return result, nil
	}
}

// NewImageSelectHandler delivers only a previously inspected request-owned
// normalized preview. Selection is idempotent and never downloads an original.
func NewImageSelectHandler() func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		if err := ctx.Err(); err != nil {
			return governance.Result{}, err
		}
		principal, _ := requestctx.PrincipalFromContext(ctx)
		if principal.Gateway == "homeassistant" {
			return governance.Result{}, errors.New("image selection is unavailable on Home Assistant")
		}
		id, _ := args["result_id"].(string)
		state := requestctx.ImageSearchStateFromContext(ctx)
		ref, err := state.InspectedReference(id)
		if err != nil {
			return governance.Result{}, err
		}
		ext := ".jpg"
		if ref.MIMEType == "image/png" {
			ext = ".png"
		} else if ref.MIMEType != "image/jpeg" {
			return governance.Result{}, errors.New("invalid preview MIME type")
		}
		if len(ref.Data) > base64.StdEncoding.EncodedLen(media.MaxNormalizedImageBytes) {
			return governance.Result{}, errors.New("preview size limit")
		}
		data, err := base64.StdEncoding.DecodeString(ref.Data)
		if err != nil || len(data) > media.MaxNormalizedImageBytes {
			return governance.Result{}, errors.New("invalid preview bytes")
		}
		attachment := media.OutputAttachment{Filename: "found-preview-" + ref.ID + ext, MIMEType: ref.MIMEType, Data: data}
		if err := attachment.Validate(); err != nil {
			return governance.Result{}, err
		}
		result := governance.Result{Content: `{"selected":true,"kind":"sourced_thumbnail_preview"}`, Outcome: governance.OutcomeProductive}
		if state.MarkSelected(id) {
			result.Attachments = []media.OutputAttachment{attachment}
		}
		return result, nil
	}
}
