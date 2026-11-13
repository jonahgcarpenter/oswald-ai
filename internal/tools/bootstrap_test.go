package tools

import (
	"testing"
)

// TestBuiltinDefinitionsStableOrder guards the catalog order through the
// workspace→tools/* surface migration: each moved tool must keep its
// original position, since order shapes the rendered prompt.
func TestBuiltinDefinitionsStableOrder(t *testing.T) {
	var names []string
	for _, def := range BuiltinDefinitions() {
		names = append(names, def.Name)
	}
	wantTail := []string{
		"read_file", "write_file", "patch", "search_files",
		"terminal", "execute_code", "process_manage", "cronjob_manage",
	}
	pos := 0
	for _, name := range names {
		if pos < len(wantTail) && name == wantTail[pos] {
			pos++
		}
	}
	if pos != len(wantTail) {
		t.Fatalf("workspace tool tail out of order in %v, want subsequence %v", names, wantTail)
	}
}
