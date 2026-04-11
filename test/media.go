package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/gorilla/websocket"
)

const (
	mediaColorReset  = "\033[0m"
	mediaColorDim    = "\033[2m"
	mediaColorYellow = "\033[33m"
	mediaColorGray   = "\033[90m"
)

type mediaStreamChunk struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mediaAgentResponse struct {
	Model    string `json:"model"`
	Response string `json:"response"`
	Thinking string `json:"thinking"`
	Error    string `json:"error"`
	Metrics  *struct {
		Model           string  `json:"model"`
		TotalDuration   int64   `json:"total_duration_ms"`
		TokensPerSecond float64 `json:"tokens_per_second"`
	} `json:"metrics"`
}

type mediaRequest struct {
	UserID      string         `json:"user_id"`
	DisplayName string         `json:"display_name"`
	Prompt      string         `json:"prompt"`
	Images      []requestImage `json:"images,omitempty"`
}

type requestImage struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
	Source   string `json:"source,omitempty"`
}

type mediaCase struct {
	Name   string
	Prompt string
	Images []requestImage
}

func main() {
	filePath := flag.String("file", "", "Path to the base image file to send")
	userID := flag.String("user", "media-test-user", "User ID for the websocket request")
	displayName := flag.String("display-name", "Media Test", "Display name for the websocket request")
	wsURL := flag.String("url", "ws://localhost:8080/ws", "WebSocket gateway URL")
	flag.Parse()

	if strings.TrimSpace(*filePath) == "" {
		log.Fatal("-file is required")
	}

	baseImage, rawBytes, err := loadImage(*filePath)
	if err != nil {
		log.Fatalf("Failed to load image: %v", err)
	}

	cases := buildCases(baseImage, rawBytes)

	parsedURL, err := url.Parse(*wsURL)
	if err != nil {
		log.Fatalf("Invalid websocket URL: %v", err)
	}

	for i, tc := range cases {
		fmt.Printf("\n=== Case %d: %s ===\n", i+1, tc.Name)
		if err := runCase(parsedURL, *userID, *displayName, tc); err != nil {
			log.Fatalf("Case %q failed: %v", tc.Name, err)
		}
	}
}

func buildCases(baseImage requestImage, rawBytes []byte) []mediaCase {
	tooLarge := makeOversizedImage(baseImage)
	nonImage := requestImage{
		MimeType: "application/pdf",
		Data:     base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 fake test document")),
		Source:   "fake.pdf",
	}

	return []mediaCase{
		{
			Name:   "image and text",
			Prompt: "What does this image say?",
			Images: []requestImage{baseImage},
		},
		{
			Name:   "image only",
			Prompt: "",
			Images: []requestImage{baseImage},
		},
		{
			Name:   "too large image",
			Prompt: "Describe the attachment situation.",
			Images: []requestImage{tooLarge},
		},
		{
			Name:   "too many images",
			Prompt: "Describe the attachment situation.",
			Images: []requestImage{
				cloneImage(baseImage, "image-1"),
				cloneImage(baseImage, "image-2"),
				cloneImage(baseImage, "image-3"),
				cloneImage(baseImage, "image-4"),
				cloneImage(baseImage, "image-5"),
			},
		},
		{
			Name:   "non-image attachment",
			Prompt: "Describe the attachment situation.",
			Images: []requestImage{nonImage},
		},
		{
			Name:   "non-image attachment only",
			Prompt: "",
			Images: []requestImage{nonImage},
		},
		{
			Name:   "mixed valid and non-image attachments",
			Prompt: "Describe everything you can about what was sent.",
			Images: []requestImage{baseImage, nonImage},
		},
		{
			Name:   "mixed valid and oversized attachments",
			Prompt: "Describe everything you can about what was sent.",
			Images: []requestImage{baseImage, tooLarge},
		},
		{
			Name:   "binary-looking non-image attachment",
			Prompt: "Describe the attachment situation.",
			Images: []requestImage{{
				MimeType: "application/octet-stream",
				Data:     base64.StdEncoding.EncodeToString(rawBytes),
				Source:   "blob.bin",
			}},
		},
	}
}

func runCase(wsURL *url.URL, userID string, displayName string, tc mediaCase) error {
	payload, err := json.Marshal(mediaRequest{
		UserID:      userID,
		DisplayName: displayName,
		Prompt:      tc.Prompt,
		Images:      tc.Images,
	})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL.String(), nil)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", wsURL.String(), err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	inThinking := false
	inContent := false

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read response: %w", err)
		}

		if chunk, ok := parseStreamChunk(raw); ok {
			switch chunk.Type {
			case "thinking":
				if !inThinking {
					fmt.Printf("%s[thinking] ", mediaColorDim)
					inThinking = true
				}
				fmt.Print(chunk.Text)
			case "content":
				if inThinking {
					fmt.Print(mediaColorReset)
					fmt.Println()
					fmt.Println()
					inThinking = false
				}
				if !inContent {
					fmt.Print(mediaColorReset)
					inContent = true
				}
				fmt.Print(chunk.Text)
			case "status":
				if inThinking || inContent {
					fmt.Print(mediaColorReset)
					fmt.Println()
					inThinking = false
					inContent = false
				}
				fmt.Printf("\n%s%s%s\n", mediaColorYellow, chunk.Text, mediaColorReset)
			}
			continue
		}

		if resp, ok := parseFinalResponse(raw); ok {
			if inThinking || inContent {
				fmt.Print(mediaColorReset)
				fmt.Println()
				fmt.Println()
			}
			if resp.Error != "" {
				fmt.Printf("%s[error] %s%s\n", mediaColorGray, resp.Error, mediaColorReset)
				return nil
			}
			if !inContent && resp.Response != "" {
				fmt.Println(resp.Response)
				fmt.Println()
			}
			fmt.Println("Final response:")
			fmt.Printf("Model: %s\n", resp.Model)
			if resp.Metrics != nil {
				fmt.Printf("%s  model=%s | %.1f tok/s | %dms%s\n",
					mediaColorGray,
					resp.Metrics.Model,
					resp.Metrics.TokensPerSecond,
					resp.Metrics.TotalDuration,
					mediaColorReset,
				)
			}
			return nil
		}

		fmt.Printf("\n[unknown] %s\n", string(raw))
	}
}

func loadImage(path string) (requestImage, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return requestImage{}, nil, err
	}

	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mimeType == "" {
		return requestImage{}, nil, fmt.Errorf("could not determine MIME type for %q", path)
	}

	return requestImage{
		MimeType: mimeType,
		Data:     base64.StdEncoding.EncodeToString(data),
		Source:   filepath.Base(path),
	}, data, nil
}

func cloneImage(base requestImage, name string) requestImage {
	clone := base
	clone.Source = fmt.Sprintf("%s-%s", name, base.Source)
	return clone
}

func makeOversizedImage(base requestImage) requestImage {
	oversizedPayload := bytes.Repeat([]byte("A"), 11<<20)
	return requestImage{
		MimeType: base.MimeType,
		Data:     base64.StdEncoding.EncodeToString(oversizedPayload),
		Source:   "oversized-" + base.Source,
	}
}

func parseStreamChunk(raw []byte) (mediaStreamChunk, bool) {
	var chunk mediaStreamChunk
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return chunk, false
	}
	return chunk, chunk.Type != ""
}

func parseFinalResponse(raw []byte) (mediaAgentResponse, bool) {
	var resp mediaAgentResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return resp, false
	}
	return resp, resp.Model != "" || resp.Response != "" || resp.Error != ""
}
