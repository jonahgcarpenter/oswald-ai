package video_analyze

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/docache"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
)

// Video source validation. data: URLs are refused (a 25 MB base64 payload is
// never the right way to hand the model a video); still images route to
// vision_analyze; GIFs keep their first-frame vision path; audio keeps the
// ffprobe/ffmpeg inspection recipe.
var (
	videoExtensions = map[string]bool{".mp4": true, ".webm": true, ".mov": true, ".m4v": true, ".mkv": true}
	audioExtensions = map[string]bool{".mp3": true, ".m4a": true, ".aac": true, ".wav": true, ".ogg": true, ".oga": true, ".opus": true, ".flac": true, ".caf": true, ".wma": true}
	imageExtensions = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".heic": true, ".heif": true, ".bmp": true, ".tiff": true}
)

type mediaCacher interface {
	Resolve(context.Context, string, string) ([]byte, string, error)
}

type downloader func(context.Context, string) ([]byte, string, error)

// NewHandler loads videos from the managed media cache or public HTTPS URLs.
func NewHandler(mediaCache *docache.Cache, ffmpeg, ffprobe string) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return newHandler(mediaCache, nil, imagecache.DownloadPublicVideo, media.ProbeMedia, ExtractFrames, ffmpeg, ffprobe)
}

// NewHandlerWithWorkspace additionally loads absolute workspace video paths
// jailed to the caller's profile root. Relative workspace paths stay rejected.
func NewHandlerWithWorkspace(mediaCache *docache.Cache, ws *workspace.Workspace, ffmpeg, ffprobe string) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return newHandler(mediaCache, ws, imagecache.DownloadPublicVideo, media.ProbeMedia, ExtractFrames, ffmpeg, ffprobe)
}

func newHandler(mediaCache mediaCacher, ws *workspace.Workspace, download downloader, probe func(context.Context, string, string) (float64, int, int), extract func(context.Context, string, string, []float64, float64, string) ([]string, error), ffmpeg, ffprobe string) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		principal, ok := requestctx.PrincipalFromContext(ctx)
		if !ok || !principal.Authenticated() || principal.Gateway == "openai" {
			return governance.Result{}, errors.New("video analysis requires an image-capable request")
		}
		if err := ctx.Err(); err != nil {
			return governance.Result{}, err
		}
		for key := range args {
			if key != "source" && key != "question" && key != "frames" && key != "timestamps" {
				return governance.Result{}, errors.New("unsupported video analysis argument")
			}
		}
		question, ok := args["question"].(string)
		question = strings.TrimSpace(question)
		if !ok || question == "" || !utf8.ValidString(question) || utf8.RuneCountInString(question) > 2000 {
			return governance.Result{}, errors.New("question must contain 1 to 2000 characters")
		}
		source, ok := args["source"].(string)
		if !ok || source == "" || source != strings.TrimSpace(source) {
			return governance.Result{}, errors.New("source must be a managed media path, absolute workspace video path, or public HTTPS video URL")
		}
		if len(source) > 8192 {
			return governance.Result{}, errors.New("source exceeds the URL/path limit")
		}
		frames, stamps, explicit, err := parseSampling(args)
		if err != nil {
			return governance.Result{}, err
		}
		if err := ctx.Err(); err != nil {
			return governance.Result{}, fmt.Errorf("video loading canceled: %w", err)
		}
		videoPath, cleanup, err := materializeSource(ctx, source, principal.CanonicalUserID, mediaCache, ws, download)
		if err != nil {
			return governance.Result{}, err
		}
		defer cleanup()
		if err := ctx.Err(); err != nil {
			return governance.Result{}, fmt.Errorf("video loading canceled: %w", err)
		}
		duration, width, height := probe(ctx, strings.TrimSpace(ffprobe), videoPath)
		if duration <= 0 || width <= 0 || height <= 0 {
			return governance.Result{}, errors.New("video is unreadable or has no picture track")
		}
		if duration > maxDurationSeconds {
			return governance.Result{}, fmt.Errorf("video exceeds the %.0f second limit — trim it with terminal ffmpeg, then re-call", maxDurationSeconds)
		}
		if !explicit {
			stamps = TimestampsFor(duration, frames)
		} else if err := checkStampRange(stamps, duration); err != nil {
			return governance.Result{}, err
		}
		for i := range stamps {
			stamps[i] = RoundStamp(stamps[i])
		}
		scratch, err := scratchDir(ws)
		if err != nil {
			return governance.Result{}, err
		}
		workdir, err := os.MkdirTemp(scratch, "video-frames-*")
		if err != nil {
			return governance.Result{}, errors.New("frame workspace is unavailable")
		}
		defer os.RemoveAll(workdir)
		if err := os.Chmod(workdir, 0700); err != nil {
			return governance.Result{}, errors.New("frame workspace is unavailable")
		}
		paths, err := extract(ctx, strings.TrimSpace(ffmpeg), videoPath, stamps, duration, workdir)
		if err != nil {
			return governance.Result{}, err
		}
		if len(paths) != len(stamps) {
			return governance.Result{}, errors.New("frame extraction produced an unexpected frame count")
		}
		raw := make([][]byte, 0, len(paths))
		for _, path := range paths {
			frame, err := ReadFrameFile(path)
			if err != nil {
				return governance.Result{}, err
			}
			raw = append(raw, frame)
		}
		images, geometries, sheets, err := assembleFrames(ctx, raw)
		if err != nil {
			return governance.Result{}, err
		}
		content := resultText(question, stamps, duration, sheets, geometries)
		return governance.Result{Content: content, Outcome: governance.OutcomeProductive, Images: images}, nil
	}
}

// parseSampling validates frames/timestamps. Timestamps win when present;
// otherwise the frames count rides along for TimestampsFor after probing.
func parseSampling(args map[string]interface{}) (frames int, stamps []float64, explicit bool, err error) {
	if raw, present := args["timestamps"]; present {
		values, ok := raw.([]interface{})
		if !ok || len(values) == 0 || len(values) > MaxFrames {
			return 0, nil, true, errors.New("timestamps must contain 1 to 8 numbers")
		}
		stamps = make([]float64, 0, len(values))
		for _, item := range values {
			var value float64
			switch n := item.(type) {
			case float64:
				value = n
			case int:
				value = float64(n)
			case json.Number:
				var convErr error
				value, convErr = n.Float64()
				if convErr != nil {
					return 0, nil, true, errors.New("timestamps must contain 1 to 8 numbers")
				}
			default:
				return 0, nil, true, errors.New("timestamps must contain 1 to 8 numbers")
			}
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return 0, nil, true, errors.New("timestamps must contain 1 to 8 numbers")
			}
			stamps = append(stamps, value)
		}
		return 0, stamps, true, nil
	}
	n := DefaultFrames
	if raw, present := args["frames"]; present {
		var value float64
		switch num := raw.(type) {
		case float64:
			value = num
		case int:
			value = float64(num)
		case json.Number:
			var convErr error
			value, convErr = num.Float64()
			if convErr != nil {
				return 0, nil, false, errors.New("frames must be an integer between 1 and 8")
			}
		default:
			return 0, nil, false, errors.New("frames must be an integer between 1 and 8")
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) || value < 1 || value > MaxFrames {
			return 0, nil, false, errors.New("frames must be an integer between 1 and 8")
		}
		n = int(value)
	}
	return n, nil, false, nil
}

// checkStampRange fails closed when an explicit timestamp falls outside the
// probed video, naming the valid range so the model can correct itself.
func checkStampRange(stamps []float64, duration float64) error {
	if len(stamps) == 0 || len(stamps) > MaxFrames {
		return errors.New("timestamps must contain 1 to 8 numbers")
	}
	for _, stamp := range stamps {
		if stamp < 0 || stamp > duration {
			return fmt.Errorf("timestamp %.1f is outside the 0 to %.1fs video duration", stamp, duration)
		}
	}
	return nil
}

// materializeSource resolves the source to a file path, downloading remote
// URLs to a private temp file. Non-video sources fail with a redirect to the
// tool that owns them. The cleanup func removes temp downloads only.
func materializeSource(ctx context.Context, source, userID string, mediaCache mediaCacher, ws *workspace.Workspace, download downloader) (path string, cleanup func(), err error) {
	noop := func() {}
	if strings.HasPrefix(source, "data:") {
		return "", noop, errors.New("video data URLs are not accepted — use a file path or URL")
	}
	if strings.HasPrefix(source, "http://") {
		return "", noop, errors.New("video URL must use HTTPS")
	}
	if strings.HasPrefix(source, "https://") {
		if download == nil {
			return "", noop, errors.New("video URL is unavailable or invalid")
		}
		data, mime, err := download(ctx, source)
		if err != nil {
			return "", noop, errors.New("video URL is unavailable or invalid")
		}
		if !strings.HasPrefix(strings.ToLower(mime), "video/") {
			return "", noop, errors.New("video URL did not return a video")
		}
		if len(data) == 0 || len(data) > maxInputBytes {
			return "", noop, errors.New("downloaded video exceeds the size limit")
		}
		tmp, err := os.CreateTemp("", "video-download-*")
		if err != nil {
			return "", noop, errors.New("video download workspace is unavailable")
		}
		tmpName := tmp.Name()
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return "", noop, errors.New("video download workspace is unavailable")
		}
		if err := tmp.Chmod(0600); err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return "", noop, errors.New("video download workspace is unavailable")
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmpName)
			return "", noop, errors.New("video download workspace is unavailable")
		}
		return tmpName, func() { os.Remove(tmpName) }, nil
	}
	if !filepath.IsAbs(source) {
		return "", noop, errors.New("unsupported video source")
	}
	if err := redirectForExtension(source); err != nil {
		return "", noop, err
	}
	if mediaCache != nil {
		if data, mime, err := mediaCache.Resolve(ctx, userID, source); err == nil {
			if !isVideoMIME(mime) && !videoExtensions[strings.ToLower(filepath.Ext(source))] {
				if redirect := redirectForMIME(mime); redirect != nil {
					return "", noop, redirect
				}
			}
			if len(data) == 0 || len(data) > maxInputBytes {
				return "", noop, errors.New("cached video exceeds the size limit")
			}
			tmp, err := os.CreateTemp("", "video-cache-*")
			if err != nil {
				return "", noop, errors.New("video workspace is unavailable")
			}
			tmpName := tmp.Name()
			if _, err := tmp.Write(data); err != nil {
				tmp.Close()
				os.Remove(tmpName)
				return "", noop, errors.New("video workspace is unavailable")
			}
			if err := tmp.Chmod(0600); err != nil {
				tmp.Close()
				os.Remove(tmpName)
				return "", noop, errors.New("video workspace is unavailable")
			}
			if err := tmp.Close(); err != nil {
				os.Remove(tmpName)
				return "", noop, errors.New("video workspace is unavailable")
			}
			return tmpName, func() { os.Remove(tmpName) }, nil
		}
	}
	if ws != nil && ws.Root() != "" {
		abs, _, err := ws.Resolve(source)
		if err != nil {
			if rel, category, ok := workspace.AsDeny(err); ok {
				return "", noop, errors.New(workspace.DenyMessage(rel, category, false))
			}
			return "", noop, errors.New("video path is unavailable or invalid")
		}
		if err := redirectForExtension(abs); err != nil {
			return "", noop, err
		}
		data, err := readBoundedVideo(abs)
		if err != nil {
			return "", noop, err
		}
		if name, mime, sniffErr := media.SniffMedia(filepath.Base(abs), "", data); sniffErr != nil || !isVideoMIME(mime) {
			_ = name
			if redirect := redirectForMIME(mime); redirect != nil {
				return "", noop, redirect
			}
			return "", noop, errors.New("video path is not a video file")
		}
		return abs, noop, nil
	}
	return "", noop, errors.New("video path is unavailable or invalid")
}

// scratchDir prefers the profile scratch dir, creating it private when the
// workspace owns it; the OS temp dir is the fallback.
func scratchDir(ws *workspace.Workspace) (string, error) {
	if ws != nil && ws.Root() != "" {
		if dir, err := ws.EnsureScratch(); err == nil {
			return dir, nil
		}
	}
	return os.TempDir(), nil
}

// videoFrame is one normalized frame with its source-geometry label and the
// raw JPEG bytes backing contact sheets (InputImage.Data is base64).
type videoFrame struct {
	image    llm.InputImage
	raw      []byte
	geometry string
}

// assembleFrames normalizes raw JPEGs and packs more than four into labeled
// contact sheets so the newest-4 image retention never drops sampled frames.
func assembleFrames(ctx context.Context, raw [][]byte) (images []llm.InputImage, geometries []string, sheets int, err error) {
	normalized := make([]videoFrame, 0, len(raw))
	for _, data := range raw {
		if err := ctx.Err(); err != nil {
			return nil, nil, 0, err
		}
		result, err := media.NormalizeVisionImage(ctx, data, "image/jpeg", nil)
		if err != nil {
			return nil, nil, 0, err
		}
		geometry := &llm.ImageGeometry{SourceWidth: result.OriginalWidth, SourceHeight: result.OriginalHeight, Width: result.Width, Height: result.Height}
		result.Image.Geometry = geometry
		normalized = append(normalized, videoFrame{image: result.Image, raw: data, geometry: geometryLabel(result.OriginalWidth, result.OriginalHeight)})
	}
	if len(normalized) <= 4 {
		for _, frame := range normalized {
			images = append(images, frame.image)
			geometries = append(geometries, frame.geometry)
		}
		return images, geometries, 1, nil
	}
	return assembleSheets(ctx, normalized)
}

// assembleSheets tiles groups of up to four normalized frames into contact
// sheets and normalizes each sheet for the model.
func assembleSheets(ctx context.Context, normalized []videoFrame) ([]llm.InputImage, []string, int, error) {
	var images []llm.InputImage
	for start := 0; start < len(normalized); start += 4 {
		end := start + 4
		if end > len(normalized) {
			end = len(normalized)
		}
		group := make([][]byte, 0, end-start)
		for _, frame := range normalized[start:end] {
			group = append(group, frame.raw)
		}
		sheet, err := ContactSheet(group)
		if err != nil {
			return nil, nil, 0, err
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, 0, err
		}
		result, err := media.NormalizeVisionImage(ctx, sheet, "image/jpeg", nil)
		if err != nil {
			return nil, nil, 0, err
		}
		geometry := &llm.ImageGeometry{SourceWidth: result.OriginalWidth, SourceHeight: result.OriginalHeight, Width: result.Width, Height: result.Height}
		result.Image.Geometry = geometry
		images = append(images, result.Image)
	}
	return images, nil, (len(normalized) + 3) / 4, nil
}

func geometryLabel(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	return itoa(width) + "×" + itoa(height)
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var buf [20]byte
	pos := len(buf)
	for value > 0 {
		pos--
		buf[pos] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// redirectForExtension routes non-video extensions to the tool that owns
// them before any bytes are read.
func redirectForExtension(path string) error {
	switch ext := strings.ToLower(filepath.Ext(path)); {
	case ext == ".gif":
		return errors.New("GIFs load their first frame with vision_analyze")
	case audioExtensions[ext]:
		return errors.New("audio keeps the terminal recipe — use ffprobe for metadata and ffmpeg to trim or transcode; you cannot listen to audio or transcribe it")
	case imageExtensions[ext]:
		return errors.New("still images belong to vision_analyze")
	case videoExtensions[ext]:
		return nil
	default:
		return errors.New("unsupported video file type")
	}
}

// redirectForMIME routes sniffed non-video payloads to the owning tool.
func redirectForMIME(mime string) error {
	lower := strings.ToLower(mime)
	switch {
	case strings.HasPrefix(lower, "image/gif"):
		return errors.New("GIFs load their first frame with vision_analyze")
	case strings.HasPrefix(lower, "audio/"):
		return errors.New("audio keeps the terminal recipe — use ffprobe for metadata and ffmpeg to trim or transcode; you cannot listen to audio or transcribe it")
	case strings.HasPrefix(lower, "image/"):
		return errors.New("still images belong to vision_analyze")
	default:
		return nil
	}
}

func isVideoMIME(mime string) bool {
	return strings.HasPrefix(strings.ToLower(mime), "video/")
}

// readBoundedVideo reads a workspace video with the same privacy and size
// discipline as workspace image reads, at the video input cap.
func readBoundedVideo(abs string) ([]byte, error) {
	f, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if info.Size() > maxInputBytes {
		return nil, errors.New("video file exceeds the size limit")
	}
	buf := make([]byte, 0, 64<<10)
	chunk := make([]byte, 32<<10)
	for {
		n, readErr := f.Read(chunk)
		if n > 0 {
			if int64(len(buf)+n) > maxInputBytes+1 {
				return nil, errors.New("video file exceeds the size limit")
			}
			buf = append(buf, chunk[:n]...)
		}
		if readErr != nil {
			break
		}
	}
	if len(buf) == 0 {
		return nil, errors.New("video file is empty")
	}
	return buf, nil
}
