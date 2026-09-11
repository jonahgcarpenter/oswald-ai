package documents

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"path"
	"strings"
)

// Archives are never unpacked on disk. Check every member, not just used XML,
// including actual inflation and CRC, before handing an office ZIP to a tool.
func openArchive(ctx context.Context, data []byte) (map[string][]byte, error) {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, ErrInvalid
	}
	if len(z.File) > 2048 {
		return nil, ErrLimit
	}
	files := make(map[string][]byte)
	seen := make(map[string]bool)
	remaining := int64(maxArchive)
	for _, f := range z.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(f.Name, "/")
		if name == "." || name == "" || path.IsAbs(name) || path.Clean(name) != name || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\:\x00") || len(name) > 512 || seen[name] {
			return nil, ErrInvalid
		}
		seen[name] = true
		if f.FileInfo().IsDir() {
			if f.UncompressedSize64 != 0 || f.CompressedSize64 != 0 {
				return nil, ErrInvalid
			}
			continue
		}
		if !f.Mode().IsRegular() || f.Flags&1 != 0 {
			return nil, ErrInvalid
		}
		if f.CompressedSize64 > uint64(len(data)) {
			return nil, ErrInvalid
		}
		if f.UncompressedSize64 > uint64(remaining) || (f.UncompressedSize64 > 1<<20 && f.UncompressedSize64 > 200*max(f.CompressedSize64, 1)) {
			return nil, ErrLimit
		}
		reader, err := f.Open()
		if err != nil {
			return nil, ErrInvalid
		}
		b, readErr := io.ReadAll(io.LimitReader(reader, remaining+1))
		closeErr := reader.Close()
		if int64(len(b)) > remaining {
			return nil, ErrLimit
		}
		if readErr != nil || closeErr != nil || uint64(len(b)) != f.UncompressedSize64 {
			return nil, ErrInvalid
		}
		remaining -= int64(len(b))
		if strings.HasSuffix(name, ".xml") || strings.HasSuffix(name, ".rels") {
			if err := validateXML(ctx, b); err != nil {
				return nil, err
			}
			files[name] = b
		}
	}
	return files, nil
}
