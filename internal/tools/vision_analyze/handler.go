package vision_analyze

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
)

type sourceCache interface {
	Resolve(context.Context, string, string) ([]byte, string, error)
}

type downloader func(context.Context, string) ([]byte, string, error)

// NewHandler loads only public downloads, bounded data URLs, and owner-only cache files.
func NewHandler(cache *imagecache.Cache) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return newHandler(cache, imagecache.DownloadPublicImage, nil)
}

// NewHandlerWithWorkspace additionally loads absolute workspace image paths
// jailed to the caller's profile root. Relative workspace paths stay rejected.
func NewHandlerWithWorkspace(cache *imagecache.Cache, ws *workspace.Workspace) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return newHandler(cache, imagecache.DownloadPublicImage, ws)
}

func newHandler(cache sourceCache, download downloader, ws *workspace.Workspace) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		principal, ok := requestctx.PrincipalFromContext(ctx)
		if !ok || !principal.Authenticated() || principal.Gateway == "openai" {
			return governance.Result{}, errors.New("vision analysis requires an authenticated image-capable request")
		}
		if err := ctx.Err(); err != nil {
			return governance.Result{}, err
		}
		for key := range args {
			if key != "image_url" && key != "question" && key != "region" {
				return governance.Result{}, errors.New("unsupported vision analysis argument")
			}
		}
		question, ok := args["question"].(string)
		question = strings.TrimSpace(question)
		if !ok || question == "" || !utf8.ValidString(question) || utf8.RuneCountInString(question) > 2000 {
			return governance.Result{}, errors.New("question must contain 1 to 2000 characters")
		}
		source, ok := args["image_url"].(string)
		if !ok || source == "" || source != strings.TrimSpace(source) {
			return governance.Result{}, errors.New("image_url must be a public URL, managed image path, absolute workspace image path, or base64 data URL")
		}
		region, err := parseRegion(args)
		if err != nil {
			return governance.Result{}, err
		}
		var data []byte
		var mime string
		switch {
		case strings.HasPrefix(source, "data:"):
			data, mime, err = decodeDataURL(source)
		case len(source) > 8192:
			err = errors.New("image_url exceeds the URL/path limit")
		case strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://"):
			data, mime, err = download(ctx, source)
		case filepath.IsAbs(source) && cache != nil:
			data, mime, err = cache.Resolve(ctx, principal.CanonicalUserID, source)
			if err != nil && ws != nil && ws.Root() != "" {
				data, mime, err = loadWorkspaceImage(source, ws)
				if err != nil {
					if rel, category, ok := workspace.AsDeny(err); ok {
						return governance.Result{}, errors.New(workspace.DenyMessage(rel, category, false))
					}
					err = errors.New("image_url is unavailable or invalid")
				}
			}
		case filepath.IsAbs(source) && ws != nil && ws.Root() != "":
			data, mime, err = loadWorkspaceImage(source, ws)
			if err != nil {
				if rel, category, ok := workspace.AsDeny(err); ok {
					return governance.Result{}, errors.New(workspace.DenyMessage(rel, category, false))
				}
				err = errors.New("image_url is unavailable or invalid")
			}
		default:
			err = errors.New("unsupported image source")
		}
		if ctx.Err() != nil {
			return governance.Result{}, fmt.Errorf("vision loading canceled: %w", ctx.Err())
		}
		if err != nil {
			return governance.Result{}, errors.New("image_url is unavailable or invalid")
		}
		normalized, err := media.NormalizeVisionImage(ctx, data, mime, region)
		if err != nil {
			return governance.Result{}, err
		}
		if err := ctx.Err(); err != nil {
			return governance.Result{}, err
		}
		geometry := &llm.ImageGeometry{SourceWidth: normalized.OriginalWidth, SourceHeight: normalized.OriginalHeight, Width: normalized.Width, Height: normalized.Height}
		if region != nil {
			geometry.Region = &[4]int{region.Min.X, region.Min.Y, region.Max.X, region.Max.Y}
		}
		normalized.Image.Geometry = geometry
		content := resultText(question, geometry, normalized.DecodedFormat == "gif")
		return governance.Result{Content: content, Outcome: governance.OutcomeProductive, Images: []llm.InputImage{normalized.Image}}, nil
	}
}

func loadWorkspaceImage(source string, ws *workspace.Workspace) ([]byte, string, error) {
	abs, _, err := ws.Resolve(source)
	if err != nil {
		return nil, "", err
	}
	data, err := readRegularImage(abs)
	if err != nil {
		return nil, "", err
	}
	mime := http.DetectContentType(data)
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return nil, "", errors.New("unsupported workspace image type")
	}
	return data, mime, nil
}

func readRegularImage(abs string) ([]byte, error) {
	f, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if info.Size() > media.MaxOutputAttachmentBytes {
		return nil, errors.New("workspace image exceeds the size limit")
	}
	buf := make([]byte, 0, 64<<10)
	chunk := make([]byte, 32<<10)
	for {
		n, readErr := f.Read(chunk)
		if n > 0 {
			if int64(len(buf)+n) > media.MaxOutputAttachmentBytes+1 {
				return nil, errors.New("workspace image exceeds the size limit")
			}
			buf = append(buf, chunk[:n]...)
		}
		if readErr != nil {
			break
		}
	}
	if len(buf) == 0 {
		return nil, errors.New("workspace image is empty")
	}
	return buf, nil
}

func decodeDataURL(source string) ([]byte, string, error) {
	if len(source) > base64.StdEncoding.EncodedLen(media.MaxOutputAttachmentBytes)+64 {
		return nil, "", errors.New("data URL exceeds the image size limit")
	}
	header, encoded, ok := strings.Cut(source, ",")
	if !ok || !strings.HasSuffix(header, ";base64") {
		return nil, "", errors.New("image data URL must use base64")
	}
	mime := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return nil, "", errors.New("unsupported image data URL type")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > media.MaxOutputAttachmentBytes {
		return nil, "", errors.New("invalid image data URL")
	}
	return data, mime, nil
}

func parseRegion(args map[string]interface{}) (*image.Rectangle, error) {
	raw, exists := args["region"]
	if !exists {
		return nil, nil
	}
	// Model JSON decodes arrays as []interface{} and numbers as float64.
	values, ok := raw.([]interface{})
	if !ok || len(values) != 4 {
		return nil, errors.New("region must contain exactly four integers")
	}
	var coords [4]int
	for i, raw := range values {
		var value float64
		switch n := raw.(type) {
		case float64:
			value = n
		case int:
			value = float64(n)
		case json.Number:
			var err error
			value, err = n.Float64()
			if err != nil {
				return nil, errors.New("region must contain exactly four integers")
			}
		default:
			return nil, errors.New("region must contain exactly four integers")
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) || value < 0 || value > 8192 {
			return nil, errors.New("region coordinates must be integers between 0 and 8192")
		}
		coords[i] = int(value)
	}
	if coords[2] <= coords[0] || coords[3] <= coords[1] {
		return nil, errors.New("region must have positive width and height")
	}
	region := image.Rect(coords[0], coords[1], coords[2], coords[3])
	return &region, nil
}
