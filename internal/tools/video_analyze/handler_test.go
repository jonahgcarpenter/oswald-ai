package video_analyze

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/docache"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
)

func videoContext(profile, gateway string) context.Context {
	return requestctx.WithPrincipal(context.Background(), identity.Principal{CanonicalUserID: profile, ExternalID: "synthetic", Gateway: gateway, Assurance: identity.AssuranceDiscordGateway})
}

func frameJPEG(width, height int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// fakeSeams wires deterministic download/probe/extract doubles. The extractor
// writes one valid JPEG per stamp so normalization runs for real.
func fakeSeams(probe func(ctx context.Context, ffprobe, path string) (float64, int, int)) (downloader, func(context.Context, string, string) (float64, int, int), func(context.Context, string, string, []float64, float64, string) ([]string, error)) {
	download := func(ctx context.Context, raw string) ([]byte, string, error) {
		return []byte("synthetic-video-bytes"), "video/mp4", nil
	}
	extract := func(ctx context.Context, ffmpeg, src string, stamps []float64, duration float64, dir string) ([]string, error) {
		paths := make([]string, 0, len(stamps))
		for i := range stamps {
			path := filepath.Join(dir, "frame.jpg")
			if len(stamps) > 1 {
				path = filepath.Join(dir, "frame-"+string(rune('0'+i))+".jpg")
			}
			if err := os.WriteFile(path, frameJPEG(64, 48), 0600); err != nil {
				return nil, err
			}
			paths = append(paths, path)
		}
		return paths, nil
	}
	return download, probe, extract
}

func stdProbe(ctx context.Context, ffprobe, path string) (float64, int, int) {
	return 13.0, 640, 480
}

func videoWorkspace(t *testing.T) (*workspace.Workspace, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return workspace.NewWorkspace(root, "default"), root
}

func TestHandlerValidation(t *testing.T) {
	mediaCache := docache.NewMedia(t.TempDir())
	ws, _ := videoWorkspace(t)
	download, probe, extract := fakeSeams(stdProbe)
	handler := newHandler(mediaCache, ws, download, probe, extract, "ffmpeg", "ffprobe")
	ctx := videoContext("alice", "discord")
	cases := []map[string]interface{}{
		{},
		{"source": "https://public.example/clip.mp4"},
		{"source": "https://public.example/clip.mp4", "question": ""},
		{"source": "https://public.example/clip.mp4", "question": "Q", "bogus": true},
		{"source": "https://public.example/clip.mp4", "question": "Q", "frames": "four"},
		{"source": "https://public.example/clip.mp4", "question": "Q", "frames": float64(0)},
		{"source": "https://public.example/clip.mp4", "question": "Q", "frames": float64(9)},
		{"source": "https://public.example/clip.mp4", "question": "Q", "timestamps": []interface{}{}},
		{"source": "https://public.example/clip.mp4", "question": "Q", "timestamps": []interface{}{float64(1), float64(2), float64(3), float64(4), float64(5), float64(6), float64(7), float64(8), float64(9)}},
		{"source": "https://public.example/clip.mp4", "question": "Q", "timestamps": []interface{}{"soon"}},
		{"source": "data:video/mp4;base64,AAAA", "question": "Q"},
		{"source": "http://public.example/clip.mp4", "question": "Q"},
		{"source": "relative/clip.mp4", "question": "Q"},
		{"source": "/tmp/photo.jpg", "question": "Q"},
		{"source": "/tmp/song.mp3", "question": "Q"},
		{"source": "/tmp/funny.gif", "question": "Q"},
	}
	for i, args := range cases {
		if _, err := handler(ctx, args); err == nil {
			t.Fatalf("case %d accepted: %v", i, args)
		}
	}
	if _, err := handler(videoContext("alice", "openai"), map[string]interface{}{"source": "https://public.example/clip.mp4", "question": "Q"}); err == nil {
		t.Fatal("stateless gateway accepted")
	}
}

func TestHandlerEvenFrames(t *testing.T) {
	mediaCache := docache.NewMedia(t.TempDir())
	ws, _ := videoWorkspace(t)
	var gotStamps []float64
	download, _, extract := fakeSeams(stdProbe)
	probe := func(ctx context.Context, ffprobe, path string) (float64, int, int) { return 13.0, 640, 480 }
	extractOnce := func(ctx context.Context, ffmpeg, src string, stamps []float64, duration float64, dir string) ([]string, error) {
		gotStamps = append([]float64(nil), stamps...)
		return extract(ctx, ffmpeg, src, stamps, duration, dir)
	}
	handler := newHandler(mediaCache, ws, download, probe, extractOnce, "ffmpeg", "ffprobe")
	result, err := handler(videoContext("alice", "discord"), map[string]interface{}{"source": "https://public.example/clip.mp4", "question": "What happens?"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != governance.OutcomeProductive || len(result.Images) != 4 || len(result.Attachments) != 0 {
		t.Fatalf("images=%d attachments=%d outcome=%v", len(result.Images), len(result.Attachments), result.Outcome)
	}
	if len(gotStamps) != 4 || gotStamps[0] <= 0 || gotStamps[3] >= 13.0 {
		t.Fatalf("stamps = %v", gotStamps)
	}
	for _, want := range []string{"4 still frames", "t=1.6s", "Question: What happens?", "not perceived", "Frame 4/4 @ 11.4s"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("missing %q in %q", want, result.Content)
		}
	}
	if geometry := result.Images[0].Geometry; geometry == nil || geometry.SourceWidth <= 0 {
		t.Fatalf("geometry = %+v", geometry)
	}
}

func TestHandlerTimestampsWin(t *testing.T) {
	mediaCache := docache.NewMedia(t.TempDir())
	ws, _ := videoWorkspace(t)
	var gotStamps []float64
	download, probe, _ := fakeSeams(stdProbe)
	extract := func(ctx context.Context, ffmpeg, src string, stamps []float64, duration float64, dir string) ([]string, error) {
		gotStamps = append([]float64(nil), stamps...)
		var paths []string
		for i := range stamps {
			path := filepath.Join(dir, "shot.jpg")
			if len(stamps) > 1 {
				path = filepath.Join(dir, "shot.jpg"+string(rune('a'+i)))
			}
			if err := os.WriteFile(path, frameJPEG(32, 24), 0600); err != nil {
				return nil, err
			}
			paths = append(paths, path)
		}
		return paths, nil
	}
	handler := newHandler(mediaCache, ws, download, probe, extract, "ffmpeg", "ffprobe")
	result, err := handler(videoContext("alice", "discord"), map[string]interface{}{
		"source": "https://public.example/clip.mp4", "question": "Q",
		"frames": float64(8), "timestamps": []interface{}{float64(7.0), float64(2.5)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotStamps) != 2 || gotStamps[0] != 7.0 || gotStamps[1] != 2.5 {
		t.Fatalf("stamps = %v", gotStamps)
	}
	if len(result.Images) != 2 || !strings.Contains(result.Content, "t=7.0s, 2.5s") {
		t.Fatalf("images=%d content=%q", len(result.Images), result.Content)
	}
	if _, err := handler(videoContext("alice", "discord"), map[string]interface{}{
		"source": "https://public.example/clip.mp4", "question": "Q", "timestamps": []interface{}{float64(99.0)},
	}); err == nil {
		t.Fatal("out-of-range timestamp accepted")
	}
}

func TestHandlerSheetsAboveFour(t *testing.T) {
	mediaCache := docache.NewMedia(t.TempDir())
	ws, _ := videoWorkspace(t)
	download, probe, extract := fakeSeams(stdProbe)
	handler := newHandler(mediaCache, ws, download, probe, extract, "ffmpeg", "ffprobe")
	result, err := handler(videoContext("alice", "discord"), map[string]interface{}{
		"source": "https://public.example/clip.mp4", "question": "Q", "frames": float64(6),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Images) != 2 {
		t.Fatalf("sheets = %d", len(result.Images))
	}
	if !strings.Contains(result.Content, "Sheet 1/2") || !strings.Contains(result.Content, "Sheet 2/2") {
		t.Fatalf("sheet map missing: %q", result.Content)
	}
}

func TestHandlerCachedAndWorkspaceSources(t *testing.T) {
	root := t.TempDir()
	mediaCache := docache.NewMedia(root)
	cached, err := mediaCache.Save(context.Background(), "alice", "clip.mp4", []byte("synthetic-video-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	ws, wsRoot := videoWorkspace(t)
	local := filepath.Join(wsRoot, "trip.mp4")
	if err := os.WriteFile(local, []byte("synthetic-video-bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	download, probe, extract := fakeSeams(stdProbe)
	handler := newHandler(mediaCache, ws, download, probe, extract, "ffmpeg", "ffprobe")
	ctx := videoContext("alice", "discord")
	for _, source := range []string{cached, local} {
		result, err := handler(ctx, map[string]interface{}{"source": source, "question": "Q"})
		if err != nil {
			t.Fatalf("source %s: %v", source, err)
		}
		if len(result.Images) != 4 {
			t.Fatalf("source %s images=%d", source, len(result.Images))
		}
	}
	if _, err := handler(ctx, map[string]interface{}{"source": filepath.Join(wsRoot, "missing.mp4"), "question": "Q"}); err == nil {
		t.Fatal("missing workspace file accepted")
	}
}

func TestHandlerProbeFailures(t *testing.T) {
	mediaCache := docache.NewMedia(t.TempDir())
	ws, _ := videoWorkspace(t)
	download, _, extract := fakeSeams(stdProbe)
	badProbe := func(ctx context.Context, ffprobe, path string) (float64, int, int) { return 0, 0, 0 }
	if _, err := newHandler(mediaCache, ws, download, badProbe, extract, "ffmpeg", "ffprobe")(videoContext("alice", "discord"), map[string]interface{}{"source": "https://public.example/clip.mp4", "question": "Q"}); err == nil {
		t.Fatal("unreadable video accepted")
	}
	longProbe := func(ctx context.Context, ffprobe, path string) (float64, int, int) { return 3600, 640, 480 }
	_, err := newHandler(mediaCache, ws, download, longProbe, extract, "ffmpeg", "ffprobe")(videoContext("alice", "discord"), map[string]interface{}{"source": "https://public.example/clip.mp4", "question": "Q"})
	if err == nil || !strings.Contains(err.Error(), "trim") {
		t.Fatalf("long video err=%v", err)
	}
	audioProbe := func(ctx context.Context, ffprobe, path string) (float64, int, int) { return 200, 0, 0 }
	if _, err := newHandler(mediaCache, ws, download, audioProbe, extract, "ffmpeg", "ffprobe")(videoContext("alice", "discord"), map[string]interface{}{"source": "https://public.example/clip.mp4", "question": "Q"}); err == nil {
		t.Fatal("audio-only stream accepted")
	}
}

func TestHandlerRouteMessages(t *testing.T) {
	mediaCache := docache.NewMedia(t.TempDir())
	ws, wsRoot := videoWorkspace(t)
	download, probe, extract := fakeSeams(stdProbe)
	handler := newHandler(mediaCache, ws, download, probe, extract, "ffmpeg", "ffprobe")
	ctx := videoContext("alice", "discord")
	imagePath := filepath.Join(wsRoot, "photo.png")
	if err := os.WriteFile(imagePath, frameJPEG(8, 8), 0600); err != nil {
		t.Fatal(err)
	}
	// Workspace stills route to vision_analyze even though bytes are absent here;
	// the extension gate fires first.
	if _, err := handler(ctx, map[string]interface{}{"source": imagePath, "question": "Q"}); err == nil || !strings.Contains(err.Error(), "vision_analyze") {
		t.Fatalf("still image err=%v", err)
	}
	gifPath := filepath.Join(wsRoot, "funny.gif")
	if err := os.WriteFile(gifPath, []byte("GIF89a"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := handler(ctx, map[string]interface{}{"source": gifPath, "question": "Q"}); err == nil || !strings.Contains(err.Error(), "first frame") {
		t.Fatalf("gif err=%v", err)
	}
	audioPath := filepath.Join(wsRoot, "voice.m4a")
	if err := os.WriteFile(audioPath, []byte("synthetic-audio"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := handler(ctx, map[string]interface{}{"source": audioPath, "question": "Q"}); err == nil || !strings.Contains(err.Error(), "ffprobe") {
		t.Fatalf("audio err=%v", err)
	}
	// A cached non-video payload probing as video still plays: probe is authority.
	if _, err := handler(ctx, map[string]interface{}{"source": "https://public.example/clip.mp4", "question": "Q", "timestamps": "soon"}); err == nil {
		t.Fatal("non-numeric timestamps accepted")
	}
}
