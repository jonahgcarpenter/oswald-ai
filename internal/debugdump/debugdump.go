package debugdump

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

var mu sync.Mutex

// WriteSection merges a named JSON section into a shared debug dump file.
// Existing sections are preserved so multiple subsystems can contribute to
// the same snapshot file without clobbering each other.
func WriteSection(path string, section string, value interface{}) error {
	if path == "" {
		return nil
	}

	mu.Lock()
	defer mu.Unlock()

	sections := make(map[string]json.RawMessage)

	if existing, err := os.ReadFile(path); err == nil && len(existing) > 0 {
		if err := json.Unmarshal(existing, &sections); err != nil {
			sections = make(map[string]json.RawMessage)
		}
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	sections[section] = encoded

	final, err := json.MarshalIndent(sections, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	return os.WriteFile(path, final, 0o644)
}
