package toolnames

import "testing"

func TestMemoryToolFamiliesUseScopeExplicitNames(t *testing.T) {
	wantUser := [4]string{"user_memory_save", "user_memory_search", "user_memory_list", "user_memory_forget"}
	wantGlobal := [4]string{"global_memory_save", "global_memory_search", "global_memory_list", "global_memory_forget"}
	if got := UserMemoryFamily(); got != wantUser {
		t.Fatalf("user memory tools = %#v, want %#v", got, wantUser)
	}
	if got := GlobalMemoryFamily(); got != wantGlobal {
		t.Fatalf("global memory tools = %#v, want %#v", got, wantGlobal)
	}
	if SessionTranscriptSearch != "session_transcript_search" {
		t.Fatalf("session transcript tool = %q", SessionTranscriptSearch)
	}
}
