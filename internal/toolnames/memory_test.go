package toolnames

import "testing"

func TestMemoryToolsUseScopeExplicitNames(t *testing.T) {
	if GlobalMemorySearch != "global_memory_search" {
		t.Fatalf("global memory search tool = %q", GlobalMemorySearch)
	}
	if SessionTranscriptSearch != "session_transcript_search" {
		t.Fatalf("session transcript tool = %q", SessionTranscriptSearch)
	}
}
