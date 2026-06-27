package media

import "testing"

func TestAttachmentLabelFallbacks(t *testing.T) {
	tests := []struct {
		name     string
		mimeType string
		want     string
	}{
		{name: " report.pdf ", mimeType: "application/pdf", want: "report.pdf"},
		{name: "", mimeType: " image/tiff ", want: "image/tiff"},
		{name: " ", mimeType: " ", want: "unknown file"},
	}

	for _, tt := range tests {
		if got := AttachmentLabel(tt.name, tt.mimeType); got != tt.want {
			t.Fatalf("AttachmentLabel(%q, %q) = %q, want %q", tt.name, tt.mimeType, got, tt.want)
		}
	}
}

func TestAugmentPromptWithUnsupportedFilesCompactsLabels(t *testing.T) {
	got := AugmentPromptWithUnsupportedFiles("  describe this  ", []string{" file.pdf ", "", "file.pdf", "archive.zip"})
	want := "describe this\n\n[User attached unsupported files: file.pdf, archive.zip]"
	if got != want {
		t.Fatalf("AugmentPromptWithUnsupportedFiles() = %q, want %q", got, want)
	}
}

func TestAugmentPromptWithUnsupportedFilesOnlyNote(t *testing.T) {
	got := AugmentPromptWithUnsupportedFiles(" ", []string{"notes.txt"})
	want := "[User attached an unsupported file: notes.txt]"
	if got != want {
		t.Fatalf("AugmentPromptWithUnsupportedFiles() = %q, want %q", got, want)
	}
}
