package main

import (
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
	imageColorReset  = "\033[0m"
	imageColorDim    = "\033[2m"
	imageColorYellow = "\033[33m"
	imageColorGray   = "\033[90m"
)

type imageStreamChunk struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type imageAgentResponse struct {
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

type imageRequest struct {
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

func main() {
	filePath := flag.String("file", "", "Path to the image file to send")
	prompt := flag.String("prompt", "What does this image say?", "Prompt to send with the image")
	userID := flag.String("user", "image-test-user", "User ID for the websocket request")
	displayName := flag.String("display-name", "Image Test", "Display name for the websocket request")
	wsURL := flag.String("url", "ws://localhost:8080/ws", "WebSocket gateway URL")
	flag.Parse()

	if strings.TrimSpace(*filePath) == "" {
		log.Fatal("-file is required")
	}

	image, err := loadImage(*filePath)
	if err != nil {
		log.Fatalf("Failed to load image: %v", err)
	}

	request := imageRequest{
		UserID:      *userID,
		DisplayName: *displayName,
		Prompt:      *prompt,
		Images:      []requestImage{image},
	}

	payload, err := json.Marshal(request)
	if err != nil {
		log.Fatalf("Failed to marshal request: %v", err)
	}

	parsedURL, err := url.Parse(*wsURL)
	if err != nil {
		log.Fatalf("Invalid websocket URL: %v", err)
	}

	conn, _, err := websocket.DefaultDialer.Dial(parsedURL.String(), nil)
	if err != nil {
		log.Fatalf("Failed to connect to %s: %v", parsedURL.String(), err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		log.Fatalf("Failed to send request: %v", err)
	}

	inThinking := false
	inContent := false

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			log.Fatalf("Failed to read response: %v", err)
		}

		if chunk, ok := parseStreamChunk(raw); ok {
			switch chunk.Type {
			case "thinking":
				if !inThinking {
					fmt.Printf("%s[thinking] ", imageColorDim)
					inThinking = true
				}
				fmt.Print(chunk.Text)
			case "content":
				if inThinking {
					fmt.Print(imageColorReset)
					fmt.Println()
					fmt.Println()
					inThinking = false
				}
				if !inContent {
					fmt.Print(imageColorReset)
					inContent = true
				}
				fmt.Print(chunk.Text)
			case "status":
				if inThinking || inContent {
					fmt.Print(imageColorReset)
					fmt.Println()
					inThinking = false
					inContent = false
				}
				fmt.Printf("\n%s%s%s\n", imageColorYellow, chunk.Text, imageColorReset)
			}
			continue
		}

		if resp, ok := parseFinalResponse(raw); ok {
			if inThinking || inContent {
				fmt.Print(imageColorReset)
				fmt.Println()
				fmt.Println()
			}
			if resp.Error != "" {
				fmt.Printf("%s[error] %s%s\n", imageColorGray, resp.Error, imageColorReset)
				return
			}
			if !inContent && resp.Response != "" {
				fmt.Println(resp.Response)
				fmt.Println()
			}
			fmt.Println("Final response:")
			fmt.Printf("Model: %s\n", resp.Model)
			if resp.Metrics != nil {
				fmt.Printf("%s  model=%s | %.1f tok/s | %dms%s\n",
					imageColorGray,
					resp.Metrics.Model,
					resp.Metrics.TokensPerSecond,
					resp.Metrics.TotalDuration,
					imageColorReset,
				)
			}
			return
		}

		fmt.Printf("\n[unknown] %s\n", string(raw))
	}
}

func loadImage(path string) (requestImage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return requestImage{}, err
	}

	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mimeType == "" {
		return requestImage{}, fmt.Errorf("could not determine MIME type for %q", path)
	}

	return requestImage{
		MimeType: mimeType,
		Data:     base64.StdEncoding.EncodeToString(data),
		Source:   filepath.Base(path),
	}, nil
}

func parseStreamChunk(raw []byte) (imageStreamChunk, bool) {
	var chunk imageStreamChunk
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return chunk, false
	}
	return chunk, chunk.Type != ""
}

func parseFinalResponse(raw []byte) (imageAgentResponse, bool) {
	var resp imageAgentResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return resp, false
	}
	return resp, resp.Model != "" || resp.Response != "" || resp.Error != ""
}
