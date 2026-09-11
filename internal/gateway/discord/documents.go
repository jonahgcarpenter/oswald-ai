package discord

import (
	"net/url"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/routing"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func (dg *Gateway) currentDocuments(attachments []Attachment) ([]Attachment, *requestctx.DocumentLoader) {
	var other []Attachment
	var docs []routing.DocumentDownload
	for _, a := range attachments {
		if !routing.IsDocumentAttachment(a.Filename, a.ContentType) {
			other = append(other, a)
			continue
		}
		docs = append(docs, routing.DocumentDownload{Filename: a.Filename, MediaType: a.ContentType, AdmissionKey: "discord:" + a.ID, URL: a.URL, Size: int64(a.Size)})
	}
	return other, routing.NewDocumentLoader(dg.httpClient(30*time.Second), docs, func(u *url.URL) bool {
		return u.User == nil && u.Scheme == "https" && (u.Port() == "" || u.Port() == "443") && (u.Hostname() == "cdn.discordapp.com" || u.Hostname() == "media.discordapp.net")
	})
}
