package documents

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

const (
	maxInput    = 20 << 20
	maxText     = 1 << 20
	maxPages    = 100
	maxOCRPages = 25
	maxArchive  = 32 << 20
	maxFile     = 32 << 20
	maxDisk     = 64 << 20
)

// ErrUnsupported includes legacy XLS, which is deliberately never converted.
var ErrUnsupported = errors.New("unsupported document format")

// ErrLimit means a structural, input, subprocess, or temporary-output limit was exceeded.
var ErrLimit = errors.New("document resource limit exceeded")

// ErrInvalid means the input cannot be safely interpreted in the selected format.
var ErrInvalid = errors.New("invalid document")

// Chunk is one text segment, with a one-based ordinal and format-specific locator.
// Method is native, ocr, or libreoffice-native/libreoffice-ocr.
type Chunk struct {
	Ordinal               int
	Text, Locator, Method string
}

// Result contains at most 1 MiB of UTF-8 text. Partial indicates omitted text,
// pages, OCR, or bounded spreadsheet repetition. Errors may accompany partial work.
type Result struct {
	Chunks  []Chunk
	Partial bool
}

// Extractor serializes all work, including conversions, through a cancelable permit.
// Construct with NewExtractor; do not copy an Extractor.
type Extractor struct {
	log    *config.Logger
	permit chan struct{}
	run    func(context.Context, string, string, ...string) ([]byte, error)
}

// NewExtractor creates an extractor; a nil logger disables measurement output.
func NewExtractor(log *config.Logger) *Extractor {
	return &Extractor{log: log, permit: make(chan struct{}, 1), run: runProcess}
}

// Extract accepts at most 20 MiB and bounds the entire invocation, including
// permit waiting, to three minutes. Filename is used only for format selection;
// it is never used as a disk path or subprocess argument. Native spreadsheet
// parsing reads stored values without evaluating formulas.
func (e *Extractor) Extract(ctx context.Context, filename, mediaType string, data []byte) (result Result, err error) {
	start := time.Now()
	defer func() {
		if e.log == nil {
			return
		}
		status := "ok"
		if result.Partial {
			status = "degraded"
		}
		if err != nil {
			status = "error"
		}
		if errors.Is(err, context.Canceled) {
			status = "ok"
		}
		n := 0
		for _, c := range result.Chunks {
			n += len(c.Text)
		}
		e.log.Server("documents").Info("documents.extraction.complete", "Document extraction completed",
			config.F("record_kind", "measurement"), config.F("status", status),
			config.F("duration_ms", time.Since(start).Milliseconds()), config.F("input_bytes", len(data)),
			config.F("text_bytes", n), config.F("chunk_count", len(result.Chunks)),
			config.F("is_partial", result.Partial), config.F("is_canceled", errors.Is(err, context.Canceled)), config.ErrorField(err))
	}()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	select {
	case e.permit <- struct{}{}:
		defer func() { <-e.permit }()
	case <-ctx.Done():
		return result, ctx.Err()
	}
	if err = ctx.Err(); err != nil {
		return
	}
	if len(data) > maxInput {
		return result, ErrLimit
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		mt, _, _ := mime.ParseMediaType(mediaType)
		ext = map[string]string{
			"text/plain":                ".txt",
			"text/markdown":             ".md",
			"text/csv":                  ".csv",
			"text/tab-separated-values": ".tsv",
			"application/json":          ".json",
			"application/xml":           ".xml",
			"text/xml":                  ".xml",
			"text/html":                 ".html",
			"application/pdf":           ".pdf",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ".xlsx",
			"application/vnd.oasis.opendocument.spreadsheet":                            ".ods",
			"application/vnd.ms-excel":                                                  ".xls",
			"application/msword":                                                        ".doc",
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ".docx",
			"application/vnd.oasis.opendocument.text":                                   ".odt",
			"application/rtf":                                                           ".rtf",
			"text/rtf":                                                                  ".rtf",
			"application/vnd.ms-powerpoint":                                             ".ppt",
			"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
			"application/vnd.oasis.opendocument.presentation":                           ".odp",
		}[mt]
		if ext == "" {
			switch strings.ToLower(filepath.Base(filename)) {
			case "dockerfile", "makefile":
				ext = ".txt"
			}
		}
	}
	switch ext {
	case ".xls":
		err = ErrUnsupported
	case ".xlsx", ".ods":
		err = spreadsheet(ctx, ext, data, &result)
	case ".pdf", ".doc", ".docx", ".odt", ".rtf", ".ppt", ".pptx", ".odp":
		if ext == ".docx" || ext == ".odt" || ext == ".pptx" || ext == ".odp" {
			if _, err = openArchive(ctx, data); err != nil {
				return
			}
		}
		var dir string
		dir, err = os.MkdirTemp("/tmp", "oswald-document-")
		if err != nil {
			return
		}
		defer func() {
			if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
				result.Partial = len(result.Chunks) > 0
			}
		}()
		input := filepath.Join(dir, "input"+ext)
		if err = os.WriteFile(input, data, 0600); err != nil {
			return
		}
		if ext != ".pdf" {
			input, err = e.convert(ctx, dir, input, ext)
			if err != nil {
				return
			}
		}
		err = e.pdf(ctx, dir, input, ext != ".pdf", &result)
	default:
		err = extractText(ctx, ext, data, &result)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil && len(result.Chunks) > 0 {
		result.Partial = true
	}
	return
}

func appendText(r *Result, text, locator, method string) bool {
	text = strings.ToValidUTF8(text, "")
	used := 0
	for _, c := range r.Chunks {
		used += len(c.Text)
	}
	left := maxText - used
	if len(text) > left {
		r.Partial = true
		text = text[:left]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	if strings.TrimSpace(text) != "" {
		r.Chunks = append(r.Chunks, Chunk{Ordinal: len(r.Chunks) + 1, Text: text, Locator: locator, Method: method})
	}
	return used+len(text) < maxText
}

func pageLocator(page int) string { return fmt.Sprintf("page:%d", page) }
