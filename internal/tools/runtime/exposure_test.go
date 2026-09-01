package runtime

import (
	"testing"
)

func TestExposureTrimsAndDeduplicatesToolNames(t *testing.T) {
	exposure := NewExposure()
	exposure.ExposeTools([]string{" github.get_issue ", "", "github.get_issue"})
	visibility := exposure.Visibility()

	if len(visibility.ExposedMCPTools) != 1 || !visibility.ExposedMCPTools["github.get_issue"] {
		t.Fatalf("unexpected exposed tools: %+v", visibility.ExposedMCPTools)
	}
	exposure.HideBuiltins(" comfyui.image_to_image ", "")
	visibility = exposure.Visibility()
	if len(visibility.HiddenBuiltins) != 1 || !visibility.HiddenBuiltins["comfyui.image_to_image"] {
		t.Fatalf("unexpected hidden builtins: %+v", visibility.HiddenBuiltins)
	}
}

func TestNilExposureVisibilityIsEmpty(t *testing.T) {
	var exposure *Exposure
	if exposure.Visibility().ExposedMCPTools != nil {
		t.Fatal("expected empty visibility")
	}
	exposure.ExposeTools([]string{"github.get_issue"})
}
