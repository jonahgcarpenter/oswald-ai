package privacy

import (
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/privacy"
)

func TestExportResultPreservesPartsAndExplainsReconstruction(t *testing.T) {
	export := privacy.Export{Parts: []privacy.ExportPart{
		{Filename: "export.json.part001", MIMEType: "application/octet-stream", Data: []byte("first")},
		{Filename: "export.json.part002", MIMEType: "application/octet-stream", Data: []byte("second")},
	}}
	result, err := exportResult(export)
	if err != nil {
		t.Fatal(err)
	}
	attachments := result.OrderedAttachments()
	if len(attachments) != 2 || attachments[0].Filename != export.Parts[0].Filename || string(attachments[0].Data) != "first" || attachments[1].Filename != export.Parts[1].Filename || string(attachments[1].Data) != "second" {
		t.Fatalf("attachments=%+v", attachments)
	}
	if !strings.Contains(result.Text, "Concatenate the parts byte-for-byte in filename order") || !strings.Contains(result.Text, "oswald.user-export.v1") {
		t.Fatalf("missing reconstruction instructions: %q", result.Text)
	}
}

func TestExportResultRetainsSingleAttachmentCompatibility(t *testing.T) {
	export := privacy.Export{Parts: []privacy.ExportPart{{Filename: "export.json", MIMEType: "application/json", Data: []byte(`{"schema":"oswald.user-export.v1"}`)}}}
	result, err := exportResult(export)
	if err != nil {
		t.Fatal(err)
	}
	if result.Attachment == nil || len(result.Attachments) != 0 || result.Attachment.Filename != "export.json" {
		t.Fatalf("single result=%+v", result)
	}
}
