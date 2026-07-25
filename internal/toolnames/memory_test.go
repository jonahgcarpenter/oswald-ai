package toolnames

import "testing"

func TestMemoryToolFamiliesUseScopeExplicitNames(t *testing.T) {
	wantGlobal := [4]string{"global_memory_save", "global_memory_search", "global_memory_list", "global_memory_forget"}
	if got := GlobalMemoryFamily(); got != wantGlobal {
		t.Fatalf("global memory tools = %#v, want %#v", got, wantGlobal)
	}
	if SessionTranscriptSearch != "session_transcript_search" {
		t.Fatalf("session transcript tool = %q", SessionTranscriptSearch)
	}
}
