// Package toolnames defines stable model-facing builtin tool names.
package toolnames

const (
	UserMemorySearch = "user_memory_search"
	UserMemoryList   = "user_memory_list"

	GlobalMemorySave   = "global_memory_save"
	GlobalMemorySearch = "global_memory_search"
	GlobalMemoryList   = "global_memory_list"
	GlobalMemoryForget = "global_memory_forget"

	SessionTranscriptSearch = "session_transcript_search"
)

// GlobalMemoryFamily returns the final global-memory tool family. Only tools with
// registered schemas and handlers are advertised to the model.
func GlobalMemoryFamily() [4]string {
	return [4]string{GlobalMemorySave, GlobalMemorySearch, GlobalMemoryList, GlobalMemoryForget}
}
