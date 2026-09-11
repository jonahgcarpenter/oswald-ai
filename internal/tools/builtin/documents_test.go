package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestDocumentToolsUseOwnTenantEvenForAdminAndBoundOutput(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	path := filepath.Join(t.TempDir(), "test.db")
	store := memorytest.NewStore(t, path, log)
	db, err := database.Open(path, log)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.SQL().Exec(`INSERT INTO account_users(canonical_user_id,is_admin) VALUES('owner',0),('admin',1)`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	docs, err := store.AcceptUserDocuments(ctx, "owner", []memory.DocumentUpload{{Filename: "private.txt", MediaType: "text/plain", Data: []byte("source")}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimUserDocumentExtraction(ctx, "test")
	if err != nil || job == nil {
		t.Fatal(err)
	}
	chunks := make([]memory.DocumentChunk, 8)
	for i := range chunks {
		chunks[i] = memory.DocumentChunk{Ordinal: i, Text: "needle " + strings.Repeat("\x01", 16000), Locator: "page", Method: "text"}
	}
	if err = store.CompleteUserDocumentExtraction(ctx, job, chunks, false); err != nil {
		t.Fatal(err)
	}
	actor := func(id string) context.Context {
		return requestctx.WithPrincipal(ctx, identity.Principal{CanonicalUserID: id, ExternalID: id, Gateway: "discord", Assurance: identity.AssuranceDiscordGateway})
	}
	for _, name := range []string{toolnames.UserDocumentList, toolnames.UserDocumentRead, toolnames.UserDocumentSearch} {
		handler := documentHandler(store, name)
		args := map[string]interface{}{"document_id": docs[0].ID, "query": "needle", "limit": 8, "scope": "global", "user_id": "owner"}
		if _, err = handler(ctx, args); err == nil {
			t.Fatal("unauthenticated call admitted")
		}
		result, callErr := handler(actor("admin"), args)
		if name == toolnames.UserDocumentRead {
			if callErr == nil {
				t.Fatal("admin read another tenant")
			}
		} else if callErr != nil || strings.Contains(result.Content, "private.txt") {
			t.Fatalf("tenant leak: %v %v", result, callErr)
		}
		result, callErr = handler(actor("owner"), args)
		if callErr != nil || len(result.Content) > documentOutputBytes {
			t.Fatalf("own read=%v size=%d", callErr, len(result.Content))
		}
		if name == toolnames.UserDocumentRead {
			var read documentReadOutput
			if err = json.Unmarshal([]byte(result.Content), &read); err != nil {
				t.Fatal(err)
			}
			if len(read.Chunks) == 0 || read.Chunks[0].Text == "" || !read.HasMore {
				t.Fatal("bounded read did not make progress")
			}
			args["offset"], args["text_offset"] = read.NextOffset, read.NextTextOffset
			next, nextErr := handler(actor("owner"), args)
			if nextErr != nil {
				t.Fatal(nextErr)
			}
			var page documentReadOutput
			if err = json.Unmarshal([]byte(next.Content), &page); err != nil {
				t.Fatal(err)
			}
			if page.Chunks[0].Text != string([]rune(chunks[read.NextOffset].Text)[read.NextTextOffset:read.NextTextOffset+utf8.RuneCountInString(page.Chunks[0].Text)]) {
				t.Fatal("handler did not apply text cursor")
			}
		}
	}
	for i := 0; i < 24; i++ {
		_, err = store.AcceptUserDocuments(ctx, "owner", []memory.DocumentUpload{{Filename: fmt.Sprintf("%02d", i) + strings.Repeat("f", 249) + ".txt", MediaType: strings.Repeat("m", 255), Data: []byte("source")}})
		if err != nil {
			t.Fatal(err)
		}
	}
	listed, err := documentHandler(store, toolnames.UserDocumentList)(actor("owner"), map[string]interface{}{"limit": 50})
	if err != nil || len(listed.Content) > documentOutputBytes {
		t.Fatalf("list exceeded envelope: %v", err)
	}
	var list documentListOutput
	if err = json.Unmarshal([]byte(listed.Content), &list); err != nil || !list.Truncated || len(list.Documents) == 0 {
		t.Fatalf("list did not return bounded metadata: %v", err)
	}
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err = Register(reg, testConfig(), store, nil, log); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{toolnames.UserDocumentList, toolnames.UserDocumentSearch, toolnames.UserDocumentRead} {
		policy, ok := reg.Policy(name)
		if !ok || policy.History.Mode != governance.HistoryMetadata || policy.History.SearchResult {
			t.Fatalf("document history policy=%+v", policy)
		}
	}
}

func TestDocumentReadRunePagingStitchesExactChunks(t *testing.T) {
	for _, text := range []string{strings.Repeat("x", 16<<10), strings.Repeat("<>&", 5400), strings.Repeat("\x01\n\"\\界", 2000), "ordinary text"} {
		for _, limit := range []int{3, 8} {
			t.Run(fmt.Sprintf("bytes=%d/limit=%d", len(text), limit), func(t *testing.T) {
				chunks := make([]memory.DocumentChunk, 8)
				for i := range chunks {
					chunks[i] = memory.DocumentChunk{Ordinal: i, Text: text, Locator: fmt.Sprintf("page %d", i), Method: "text"}
				}
				stitched := make([]string, len(chunks))
				offset, textOffset := 0, 0
				for pages := 0; ; pages++ {
					if pages > 100 {
						t.Fatal("paging did not terminate")
					}
					end := min(offset+limit, len(chunks))
					read := memory.DocumentRead{Document: memory.UserDocument{ID: "doc", Filename: "reference.txt", ChunkCount: 8}, Chunks: chunks[offset:end], NextOffset: end, HasMore: end < len(chunks)}
					data, err := encodeDocumentRead(read, textOffset)
					if err != nil {
						t.Fatal(err)
					}
					if len(data) > documentOutputBytes || !utf8.Valid(data) {
						t.Fatal("invalid encoded envelope")
					}
					if strings.Contains(string(data), `\u003c`) {
						t.Fatal("optional HTML escaping inflated output")
					}
					var out documentReadOutput
					if err = json.Unmarshal(data, &out); err != nil {
						t.Fatal(err)
					}
					if len(out.Chunks) == 0 {
						t.Fatal("page made no progress")
					}
					for _, chunk := range out.Chunks {
						if chunk.Text == "" || chunk.Locator != chunks[chunk.Ordinal].Locator || chunk.Method != "text" {
							t.Fatal("lost text or source metadata")
						}
						stitched[chunk.Ordinal] += chunk.Text
					}
					// A single bounded result fits the default usable input budget with
					// representative request and native tool-message framing.
					messages := []llm.ChatMessage{{Role: "system", Content: strings.Repeat("policy ", 1000)}, {Role: "user", Content: "read document"}, {Role: "tool", Content: string(data), ToolCallID: "read"}}
					if budget.EstimateRequest(messages, nil) >= budget.NewContextBudget(0, 0).UsableInputLimit() {
						t.Fatal("bounded result exceeded default request budget")
					}
					if !out.HasMore {
						if out.NextOffset != len(chunks) || out.NextTextOffset != 0 {
							t.Fatal("invalid terminal cursor")
						}
						break
					}
					if out.NextOffset < offset || (out.NextOffset == offset && out.NextTextOffset <= textOffset) {
						t.Fatal("cursor did not advance")
					}
					if text == "ordinary text" && (out.NextOffset != end || out.NextTextOffset != 0 || out.Partial) {
						t.Fatal("ordinary chunk paging changed")
					}
					offset, textOffset = out.NextOffset, out.NextTextOffset
				}
				for i := range chunks {
					if stitched[i] != chunks[i].Text {
						t.Fatalf("chunk %d lost or duplicated text", i)
					}
				}
			})
		}
	}
	read := memory.DocumentRead{Chunks: []memory.DocumentChunk{{Text: "abc"}}}
	if _, err := encodeDocumentRead(read, 3); err == nil {
		t.Fatal("out-of-range text cursor accepted")
	}
}
