package imessage

import (
	"net/url"

	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/routing"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func (g *Gateway) currentDocuments(attachments []attachment) ([]attachment, *requestctx.DocumentLoader) {
	var other []attachment
	var docs []routing.DocumentDownload
	origin, _ := url.Parse(g.BlueBubblesURL)
	for _, a := range attachments {
		if !routing.IsDocumentAttachment(a.TransferName, a.MimeType) {
			other = append(other, a)
			continue
		}
		endpoint, _ := buildBlueBubblesAttachmentEndpoint(g.BlueBubblesURL, a.GUID, g.BlueBubblesPassword)
		docs = append(docs, routing.DocumentDownload{Filename: a.TransferName, MediaType: a.MimeType, AdmissionKey: "imessage:" + a.GUID, URL: endpoint, Size: int64(a.TotalBytes)})
	}
	return other, routing.NewDocumentLoader(g.httpClient(), docs, func(u *url.URL) bool {
		return origin != nil && u.User == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Scheme == origin.Scheme && u.Host == origin.Host
	})
}
