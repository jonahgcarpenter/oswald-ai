package readfile

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

// Name is the model-facing tool name. The value is the persisted contract;
// only the owning package moved.
const Name = "read_file"

var ocrAvailable bool

// SetOCRAvailable toggles the hosted-OCR description override. It is a
// process-wide capability flag set once at startup; tests reset it.
func SetOCRAvailable(available bool) { ocrAvailable = available }

// OCRAvailable reports the hosted-OCR capability flag.
func OCRAvailable() bool { return ocrAvailable }

// DynamicDescription returns the per-request description, applying capability
// overrides to the static definition text.
func DynamicDescription(name, base string) string {
	if name == Name && ocrAvailable {
		return replaceFirst(base, "PDF (text layer)", "PDF (scanned or text)")
	}
	return base
}

func replaceFirst(s, old, new string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + new + s[i+len(old):]
		}
	}
	return s
}

// Definition returns the paginated text-file reader contract.
func Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        Name,
		Description: "Read a text file with line numbers and pagination. Use this instead of cat/head/tail in terminal. Output format: 'LINE_NUM|CONTENT'. Suggests similar filenames if not found. Use offset and limit for large files. Reads exceeding ~100K characters are truncated on a line boundary and return a next_offset; continue with offset to read the rest. Documents auto-extract to readable text: .ipynb, Office (.docx/.xlsx/.pptx and legacy .doc/.ppt/.xls), PDF (text layer), OpenDocument, RTF, EPUB, SQLite (.db/.sqlite: schema, row counts, first rows). Inbound attachments arrive as private cache paths named in the prompt — pass that path back here to read them. Cannot read images — use vision_analyze for images.",
		Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{
			"path":   {Type: "string", Description: "Path to the file to read (absolute, relative, or ~/path)"},
			"offset": {Type: "integer", Description: "Line number to start reading from (1-indexed, default: 1)", Default: 1, Minimum: float64Ptr(1)},
			"limit":  {Type: "integer", Description: "Maximum number of lines to read (default: 2000, max: 2000). Reads are additionally capped at a ~100K-character budget with a next_offset continuation.", Default: 2000, Minimum: float64Ptr(1), Maximum: float64Ptr(2000)},
		}, Required: []string{"path"}, AdditionalProperties: boolPtr(false)},
	}
}

func float64Ptr(v float64) *float64 { return &v }

func boolPtr(v bool) *bool { return &v }
