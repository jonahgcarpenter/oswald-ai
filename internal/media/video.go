package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

const videoExtractionTimeout = 20 * time.Second

// VideoFrameExtractor converts a short video payload into one normalized contact-sheet image.
type VideoFrameExtractor interface {
	Extract(ctx context.Context, data []byte, source string) (llm.InputImage, error)
}

// FFmpegVideoFrameExtractor extracts representative video frames with ffmpeg and ffprobe.
type FFmpegVideoFrameExtractor struct {
	FFmpegPath  string
	FFprobePath string
}

// Extract samples a video at four evenly spaced timestamps and returns a normalized contact sheet.
func (e FFmpegVideoFrameExtractor) Extract(ctx context.Context, data []byte, source string) (llm.InputImage, error) {
	if len(data) == 0 {
		return llm.InputImage{}, fmt.Errorf("video payload is empty")
	}
	if len(data) > MaxImageBytes {
		return llm.InputImage{}, fmt.Errorf("video payload exceeds %d bytes", MaxImageBytes)
	}

	ctx, cancel := context.WithTimeout(ctx, videoExtractionTimeout)
	defer cancel()
	tempDir, err := os.MkdirTemp("", "oswald-video-*")
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("create video extraction directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	inputPath := filepath.Join(tempDir, "input")
	if err := os.WriteFile(inputPath, data, 0o600); err != nil {
		return llm.InputImage{}, fmt.Errorf("write video payload: %w", err)
	}

	ffprobePath := strings.TrimSpace(e.FFprobePath)
	if ffprobePath == "" {
		ffprobePath = "ffprobe"
	}
	durationOutput, err := exec.CommandContext(ctx, ffprobePath,
		"-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", inputPath,
	).Output()
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("probe video duration: %w", err)
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(durationOutput)), 64)
	if err != nil || duration <= 0 {
		return llm.InputImage{}, fmt.Errorf("probe video duration returned %q", strings.TrimSpace(string(durationOutput)))
	}

	ffmpegPath := strings.TrimSpace(e.FFmpegPath)
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	outputPattern := filepath.Join(tempDir, "frame-%d.png")
	frameRate := 4 / duration
	command := exec.CommandContext(ctx, ffmpegPath,
		"-v", "error", "-i", inputPath,
		"-vf", "fps="+strconv.FormatFloat(frameRate, 'f', 9, 64),
		"-frames:v", "4", outputPattern,
	)
	if output, commandErr := command.CombinedOutput(); commandErr != nil {
		return llm.InputImage{}, fmt.Errorf("extract video frames: %w: %s", commandErr, strings.TrimSpace(string(output)))
	}

	framePaths, err := filepath.Glob(filepath.Join(tempDir, "frame-*.png"))
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("list extracted video frames: %w", err)
	}
	sort.Strings(framePaths)
	if len(framePaths) == 0 {
		return llm.InputImage{}, fmt.Errorf("ffmpeg produced no video frames")
	}
	if len(framePaths) > 4 {
		framePaths = framePaths[:4]
	}

	frames := make([]image.Image, 0, len(framePaths))
	frameWidth, frameHeight := 0, 0
	for index, framePath := range framePaths {
		frameData, err := os.ReadFile(framePath)
		if err != nil {
			return llm.InputImage{}, fmt.Errorf("read video frame %d: %w", index+1, err)
		}
		frame, _, err := image.Decode(bytes.NewReader(frameData))
		if err != nil {
			return llm.InputImage{}, fmt.Errorf("decode video frame %d: %w", index+1, err)
		}
		if index == 0 {
			frameWidth = frame.Bounds().Dx()
			frameHeight = frame.Bounds().Dy()
		} else if frame.Bounds().Dx() != frameWidth || frame.Bounds().Dy() != frameHeight {
			return llm.InputImage{}, fmt.Errorf("video frame dimensions changed from %dx%d to %dx%d", frameWidth, frameHeight, frame.Bounds().Dx(), frame.Bounds().Dy())
		}
		frames = append(frames, frame)
	}

	sheet := buildContactSheet(frames, frameWidth, frameHeight)
	_, _, encoded, mimeType, err := normalizeEncodedImage(sheet, MaxNormalizedImageLongEdge, MaxNormalizedImageBytes)
	if err != nil {
		return llm.InputImage{}, err
	}
	return llm.InputImage{MimeType: mimeType, Data: base64.StdEncoding.EncodeToString(encoded), Source: source, IsGIFContactSheet: true}, nil
}
