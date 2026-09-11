package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

const documentOutputBytes = 16 << 10

type documentListOutput struct {
	Documents []memory.UserDocument `json:"documents"`
	Truncated bool                  `json:"truncated"`
}

// NextTextOffset counts runes within NextOffset's chunk, never encoded bytes.
type documentReadOutput struct {
	memory.DocumentRead
	NextTextOffset int
	Partial        bool
}

func encodeDocumentOutput(value interface{}) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(value)
	return buffer.Bytes(), err
}

func encodeDocumentRead(read memory.DocumentRead, textOffset int) ([]byte, error) {
	if textOffset > 0 && (len(read.Chunks) == 0 || textOffset >= utf8.RuneCountInString(read.Chunks[0].Text)) {
		return nil, fmt.Errorf("text_offset must identify a rune in the starting chunk")
	}
	out := documentReadOutput{DocumentRead: read, Partial: textOffset > 0}
	out.Chunks = nil
	for i, chunk := range read.Chunks {
		start := 0
		if i == 0 {
			start = textOffset
		}
		text := []rune(chunk.Text)[start:]
		chunk.Text = string(text)
		out.Chunks = append(out.Chunks, chunk)
		out.NextOffset, out.NextTextOffset = chunk.Ordinal+1, 0
		out.HasMore = read.HasMore || i+1 < len(read.Chunks)
		data, err := encodeDocumentOutput(out)
		if err != nil {
			return nil, err
		}
		if len(data) <= documentOutputBytes {
			continue
		}

		// Find the longest rune prefix whose complete JSON envelope fits. Cursor
		// digit growth and escaping are included in every candidate measurement.
		out.Partial, out.HasMore = true, true
		out.NextOffset = chunk.Ordinal
		low, high := 0, len(text)-1
		for low < high {
			mid := low + (high-low+1)/2
			out.Chunks[i].Text = string(text[:mid])
			out.NextTextOffset = start + mid
			data, err = encodeDocumentOutput(out)
			if err != nil {
				return nil, err
			}
			if len(data) <= documentOutputBytes {
				low = mid
			} else {
				high = mid - 1
			}
		}
		if low == 0 {
			if i == 0 {
				return nil, fmt.Errorf("document metadata leaves no room for text")
			}
			out.Chunks = out.Chunks[:i]
			out.Partial = textOffset > 0
			out.NextTextOffset = 0
		} else {
			out.Chunks[i].Text = string(text[:low])
			out.NextTextOffset = start + low
		}
		break
	}
	data, err := encodeDocumentOutput(out)
	if err == nil && len(data) > documentOutputBytes {
		return nil, fmt.Errorf("document metadata exceeds output bound")
	}
	return data, err
}

func documentHandler(store *memory.Store, name string) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		principal, ok := requestctx.PrincipalFromContext(ctx)
		if !ok || !principal.Authenticated() {
			return governance.Result{}, fmt.Errorf("authenticated document owner required")
		}
		if store == nil {
			return governance.Result{}, fmt.Errorf("document storage unavailable")
		}
		limit, maximum := 5, 10
		if name == toolnames.UserDocumentRead {
			limit, maximum = 3, 8
		}
		if name == toolnames.UserDocumentList {
			limit, maximum = 20, 50
		}
		if raw, exists := args["limit"]; exists {
			n, valid := numericInt(raw)
			if !valid || n < 1 || n > maximum {
				return governance.Result{}, fmt.Errorf("limit must be between 1 and %d", maximum)
			}
			limit = n
		}
		id, _ := args["document_id"].(string)
		if len(id) > 256 {
			return governance.Result{}, fmt.Errorf("invalid document ID")
		}
		var value interface{}
		var err error
		switch name {
		case toolnames.UserDocumentList:
			docs, listErr := store.ListUserDocuments(ctx, memory.DocumentScope{UserID: principal.CanonicalUserID})
			if listErr != nil {
				return governance.Result{}, listErr
			}
			value = documentListOutput{docs[:min(limit, len(docs))], len(docs) > limit}
		case toolnames.UserDocumentSearch:
			query, _ := args["query"].(string)
			if strings.TrimSpace(query) == "" || !utf8.ValidString(query) || utf8.RuneCountInString(query) > 400 || len(query) > 1024 {
				return governance.Result{}, fmt.Errorf("query must contain 1 to 400 characters")
			}
			value, err = store.SearchUserDocuments(ctx, principal.CanonicalUserID, query, id, limit)
		case toolnames.UserDocumentRead:
			if strings.TrimSpace(id) == "" {
				return governance.Result{}, fmt.Errorf("document_id is required")
			}
			offset := 0
			if raw, exists := args["offset"]; exists {
				n, valid := numericInt(raw)
				if !valid || n < 0 || n > 1000000 {
					return governance.Result{}, fmt.Errorf("invalid chunk offset")
				}
				offset = n
			}
			textOffset := 0
			if raw, exists := args["text_offset"]; exists {
				n, valid := numericInt(raw)
				if !valid || n < 0 || n > memory.DocumentMaxChunkBytes {
					return governance.Result{}, fmt.Errorf("invalid text offset")
				}
				textOffset = n
			}
			read, readErr := store.ReadUserDocument(ctx, principal.CanonicalUserID, id, offset, limit)
			if readErr != nil {
				return governance.Result{}, readErr
			}
			data, encodeErr := encodeDocumentRead(read, textOffset)
			if encodeErr != nil {
				return governance.Result{}, encodeErr
			}
			return governance.Result{Content: string(data), Outcome: governance.OutcomeProductive}, nil
		}
		if err != nil {
			return governance.Result{}, err
		}
		data, err := encodeDocumentOutput(value)
		if err != nil {
			return governance.Result{}, err
		}
		for len(data) > documentOutputBytes {
			switch v := value.(type) {
			case documentListOutput:
				if len(v.Documents) <= 1 {
					return governance.Result{}, fmt.Errorf("document metadata exceeds output bound")
				}
				v.Documents = v.Documents[:len(v.Documents)-1]
				v.Truncated = true
				value = v
			case []memory.DocumentSearchResult:
				if len(v) <= 1 {
					return governance.Result{}, fmt.Errorf("document result exceeds output bound")
				}
				value = v[:len(v)-1]
			default:
				return governance.Result{Content: `{"status":"bounded","message":"Result too large; reduce limit or narrow the query."}`, Outcome: governance.OutcomeUnproductive}, nil
			}
			data, err = encodeDocumentOutput(value)
			if err != nil {
				return governance.Result{}, err
			}
		}
		return governance.Result{Content: string(data), Outcome: governance.OutcomeProductive}, nil
	}
}
