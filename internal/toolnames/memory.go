// Package toolnames defines stable model-facing builtin tool names.
package toolnames

const (
	UserMemorySave   = "user_memory_save"
	UserMemorySearch = "user_memory_search"
	UserMemoryList   = "user_memory_list"
	UserMemoryForget = "user_memory_forget"

	GlobalMemorySave   = "global_memory_save"
	GlobalMemorySearch = "global_memory_search"
	GlobalMemoryList   = "global_memory_list"
	GlobalMemoryForget = "global_memory_forget"

	SessionTranscriptSearch = "session_transcript_search"
)

// UserMemoryFamily returns the final user-memory tool family.
func UserMemoryFamily() [4]string {
	return [4]string{UserMemorySave, UserMemorySearch, UserMemoryList, UserMemoryForget}
}

// GlobalMemoryFamily returns the final global-memory tool family. Only tools with
// registered schemas and handlers are advertised to the model.
func GlobalMemoryFamily() [4]string {
	return [4]string{GlobalMemorySave, GlobalMemorySearch, GlobalMemoryList, GlobalMemoryForget}
}
