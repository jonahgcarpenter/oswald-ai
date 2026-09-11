package requestctx

import "context"

// DocumentUpload is a transport-neutral current attachment, not authored evidence.
type DocumentUpload struct {
	Filename, MediaType, AdmissionKey string
	Data                              []byte
}

// DocumentLoader declares reservation bounds before downloading current attachments.
// SourceBytes is a conservative ceiling; Load must not return more bytes or files.
type DocumentLoader struct {
	FileCount   int
	SourceBytes int64
	Load        func(context.Context) ([]DocumentUpload, error)
}
