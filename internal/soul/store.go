// Package soul loads Oswald's operator-managed system prompt.
package soul

import "os"

// Store reads the operator-managed soul file used as the agent's system prompt.
type Store struct {
	path string
}

// NewStore creates a read-only Store for the soul file at the given path.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Read returns the full contents of the soul file. Returns an empty string
// (not an error) if the file does not yet exist.
func (s *Store) Read() (string, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}
