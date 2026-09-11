//go:build !linux

package memory

// Unsupported platforms fail closed for file-backed uploads, not for chat or
// in-memory stores. Add a native available-to-user probe when supporting one.
func documentFilesystemFreeBytes(string) (int64, error) {
	return 0, ErrDocumentDiskSpace
}
