//go:build linux

package memory

import (
	"math"
	"syscall"
)

func documentFilesystemFreeBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, ErrDocumentDiskSpace
	}
	if stat.Bsize <= 0 {
		return 0, ErrDocumentDiskSpace
	}
	if stat.Bavail > uint64(math.MaxInt64)/uint64(stat.Bsize) {
		return math.MaxInt64, nil
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}
