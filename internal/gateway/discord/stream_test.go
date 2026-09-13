package discord

import (
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
)

func TestImageToolStatusesArePurposeSpecificAndSafe(t *testing.T) {
	for _, name := range []string{toolnames.WebImageSearch, toolnames.WebImageSelect} {
		status := discordToolStatusFor(&agent.ToolStreamPayload{
			Name: name,
			Arguments: map[string]interface{}{
				"query": "cats", "result_id": "private-id", "url": "https://private.example",
				"title": "private-title", "bytes": "private-bytes",
			},
			ResultText: "private-result",
		})
		want := []string{"Searching images for \"cats\"...", "Searched images for \"cats\".", "Image search failed for \"cats\"."}
		if name == toolnames.WebImageSelect {
			want = []string{"Selecting found image preview...", "Selected found image preview.", "Found image preview selection failed."}
		}
		for i, got := range []string{status.running, status.completed, status.failed} {
			if got != want[i] {
				t.Errorf("%s status = %q, want %q", name, got, want[i])
			}
		}
	}
}

func TestImageSearchStatusBoundsAndSanitizesQueryLikeWebSearch(t *testing.T) {
	query := "@everyone\n\x00" + strings.Repeat("cats", 100)
	status := discordToolStatusFor(&agent.ToolStreamPayload{
		Name: toolnames.WebImageSearch, Arguments: map[string]interface{}{"query": query},
	})
	web := discordToolStatusFor(&agent.ToolStreamPayload{
		Name: toolnames.WebSearch, Arguments: map[string]interface{}{"query": query},
	})
	if status.running != strings.Replace(web.running, "Searching the web", "Searching images", 1) {
		t.Fatalf("image search query formatting differs: %q", status.running)
	}
	if strings.Contains(status.running, "@everyone") || strings.ContainsAny(status.running, "\n\x00") || len(status.running) > 200 {
		t.Fatalf("unbounded or unsanitized query: %q", status.running)
	}
}
