package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/image_generate/comfy_ui"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools"
	imagegenerate "github.com/jonahgcarpenter/oswald-ai/internal/tools/image_generate"
)

func TestImageGovernanceUsesExplicitCatalogSelector(t *testing.T) {
	for _, test := range []struct {
		name    string
		args    []map[string]interface{}
		uploads []image.Point
		text    int
		blocked int
	}{
		{
			name:    "same explicit selector is duplicate",
			args:    []map[string]interface{}{{"image_url": "current-1"}, {"image_url": "current-1"}},
			uploads: []image.Point{image.Pt(2, 3)}, blocked: 1,
		},
		{
			name: "invalid selector values never submit",
			args: []map[string]interface{}{{"image_url": ""}, {"image_url": nil}, {"image_url": 1}},
		},
		{
			name:    "same source changed aspect ratio permits retry but same ratio blocks",
			args:    []map[string]interface{}{{"image_url": "current-1", "aspect_ratio": "square"}, {"image_url": "current-1", "aspect_ratio": "portrait"}, {"image_url": "current-1", "aspect_ratio": "portrait"}},
			uploads: []image.Point{image.Pt(2, 3), image.Pt(2, 3)}, blocked: 1,
		},
		{
			name:    "distinct explicit sources and exact duplicate",
			args:    []map[string]interface{}{{"image_url": "current-1"}, {"image_url": "current-2"}, {"image_url": "current-1"}},
			uploads: []image.Point{image.Pt(2, 3), image.Pt(4, 5)}, blocked: 1,
		},
		{
			name: "omitted selector always generates from text and duplicate is blocked",
			args: []map[string]interface{}{{}, {}, {}},
			text: 1, blocked: 2,
		},
		{
			name:    "omitted and explicit selectors have different fingerprints",
			args:    []map[string]interface{}{{}, {"image_url": "current-1"}, {"image_url": "current-2"}, {"image_url": "current-2"}},
			uploads: []image.Point{image.Pt(2, 3), image.Pt(4, 5)}, text: 1, blocked: 1,
		},
		{
			name:    "invalid explicit selectors still reach validation",
			args:    []map[string]interface{}{{"image_url": "current-1"}, {"image_url": ""}, {"image_url": " current-1 "}, {"image_url": nil}, {"image_url": 7}, {"image_url": "current-2"}},
			uploads: []image.Point{image.Pt(2, 3), image.Pt(4, 5)},
		},
	} {
		for _, batch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/batch=%t", test.name, batch), func(t *testing.T) {
				output := testInputImage(t, 6, 7)
				data, err := base64.StdEncoding.DecodeString(output.Data)
				if err != nil {
					t.Fatal(err)
				}
				var mu sync.Mutex
				var uploads []image.Point
				submissions := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					switch r.URL.Path {
					case "/upload/image":
						if err := r.ParseMultipartForm(1 << 20); err != nil {
							t.Error(err)
							http.Error(w, "bad upload", 400)
							return
						}
						defer r.MultipartForm.RemoveAll()
						file, _, err := r.FormFile("image")
						if err != nil {
							t.Error(err)
							http.Error(w, "missing image", 400)
							return
						}
						defer file.Close()
						decoded, _, err := image.Decode(file)
						if err != nil {
							t.Error(err)
							http.Error(w, "invalid image", 400)
							return
						}
						uploads = append(uploads, decoded.Bounds().Size())
						_ = json.NewEncoder(w).Encode(map[string]string{"name": comfy_ui.InputFilename, "subfolder": comfy_ui.InputSubfolder, "type": "input"})
					case "/prompt":
						submissions++
						_, _ = w.Write([]byte(`{"prompt_id":"job"}`))
					case "/history/job":
						_, _ = w.Write([]byte(`{"job":{"outputs":{"10":{"images":[{"filename":"result.jpg","type":"output"}]},"11":{"images":[{"filename":"result.jpg","type":"output"}]}}}}`))
					case "/view":
						w.Header().Set("Content-Type", output.MimeType)
						_, _ = w.Write(data)
					case "/free":
						w.WriteHeader(http.StatusOK)
					default:
						t.Errorf("unexpected endpoint %s", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				log := config.NewLogger(config.LevelError)
				cache := imagecache.New(t.TempDir())
				reg, err := tools.NewRegistryWithImageCache(&config.Config{
					ComfyUIURL: server.URL, ComfyUIGenerationTimeout: time.Second,
				}, nil, nil, cache, log)
				if err != nil {
					t.Fatal(err)
				}
				chat := &fakeChatter{}
				var calls []llm.ToolCall
				var original []byte
				chat.onChat = func(req llm.ChatRequest) {
					if original != nil {
						return
					}
					for _, message := range req.Messages {
						if !strings.HasPrefix(message.Content, imageContextPrefix) {
							continue
						}
						lines := strings.Split(message.Content, "\n")
						for _, call := range calls {
							switch call.Function.Arguments["image_url"] {
							case "current-1":
								call.Function.Arguments["image_url"] = strings.Split(lines[2], " (")[0]
							case "current-2":
								call.Function.Arguments["image_url"] = strings.Split(lines[3], " (")[0]
							case " current-1 ":
								call.Function.Arguments["image_url"] = " " + strings.Split(lines[2], " (")[0] + " "
							}
						}
						break
					}
					original, _ = json.Marshal(calls)
				}
				for i, source := range test.args {
					args := map[string]interface{}{"prompt": "make it blue"}
					for key, value := range source {
						args[key] = value
					}
					calls = append(calls, llm.ToolCall{ID: fmt.Sprint(i), Function: llm.ToolFunction{Name: imagegenerate.Name, Arguments: args}})
				}
				if batch {
					chat.responses = append(chat.responses, &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: calls}})
				} else {
					for _, call := range calls {
						chat.responses = append(chat.responses, &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{call}}})
					}
				}
				chat.responses = append(chat.responses, &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", Content: "Finished."}})
				a, _ := newTestAgent(t, chat, nil, reg)
				a.SetImageCache(cache)
				response, err := processAgent(a, "source-governance", "discord", "session", "user-1", "User", "edit these images", []llm.InputImage{testInputImage(t, 2, 3), testInputImage(t, 4, 5)}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if response.ToolExecutionCount != len(test.args)-test.blocked || response.ToolBlockedCount != test.blocked {
					t.Fatalf("executions=%d blocked=%d", response.ToolExecutionCount, response.ToolBlockedCount)
				}
				mu.Lock()
				defer mu.Unlock()
				if submissions != len(test.uploads)+test.text || len(uploads) != len(test.uploads) {
					t.Fatalf("submissions=%d uploads=%v want=%v", submissions, uploads, test.uploads)
				}
				for i, want := range test.uploads {
					if uploads[i] != want {
						t.Fatalf("upload %d size=%v want=%v", i, uploads[i], want)
					}
				}
				after, _ := json.Marshal(calls)
				if !bytes.Equal(original, after) {
					t.Fatal("fingerprinting mutated model arguments")
				}
			})
		}
	}
}
