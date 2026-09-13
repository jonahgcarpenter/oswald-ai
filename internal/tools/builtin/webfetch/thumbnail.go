package webfetch

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

// DownloadThumbnail uses the same public-IP dial and redirect boundary as Fetch,
// with no provider credentials or proxy. Query bytes are preserved, not search-
// result URL normalized. It never falls back to an original or oEmbed URL.
func (c *Client) DownloadThumbnail(ctx context.Context, rawURL string) (llm.InputImage, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	target, err := validateURL(rawURL)
	if err != nil {
		return llm.InputImage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return llm.InputImage{}, errors.New("invalid thumbnail request")
	}
	req.Header.Set("Accept", "image/jpeg, image/png, image/webp")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return llm.InputImage{}, ctx.Err()
		}
		return llm.InputImage{}, errors.New("thumbnail download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return llm.InputImage{}, &fetchHTTPError{status: resp.StatusCode, message: "thumbnail download rejected"}
	}
	if resp.ContentLength > media.MaxSearchPreviewBytes {
		return llm.InputImage{}, errors.New("thumbnail body limit")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxSearchPreviewBytes+1))
	if ctx.Err() != nil {
		return llm.InputImage{}, ctx.Err()
	}
	if err != nil {
		return llm.InputImage{}, errors.New("thumbnail read failed")
	}
	return media.NormalizeSearchPreview(body, resp.Header.Get("Content-Type"))
}
