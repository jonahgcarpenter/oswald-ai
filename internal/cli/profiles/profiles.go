// Package profiles implements the `oswald profile` command group: explicit,
// operator-driven provisioning and removal of private profile directories.
package profiles

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// Exit codes mirror the parent cli convention without importing it.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// maxSoulBytes bounds a copied operator soul, matching the runtime read limit.
const maxSoulBytes = 1 << 20

// defaultProfileConfig is a minimal valid override document. Empty overrides
// inherit every global provider, model, tool, and MCP default.
const defaultProfileConfig = "# Optional profile-local overrides: providers, model, tools, mcp.\n{}\n"

// Run dispatches the profile command group.
func Run(ctx context.Context, root string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		profileUsage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "create":
		return create(ctx, root, args[1:], stdout, stderr)
	case "delete":
		return deleteProfile(ctx, root, args[1:], stdin, stdout, stderr)
	case "help", "-h", "--help":
		profileUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown profile command %q\n", args[0])
		profileUsage(stderr)
		return exitUsage
	}
}

func profileUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald profile <create|delete> <name>")
}

// resolveRoot returns an absolute configuration root with no symlinked or
// non-directory path components. The root must already exist.
func resolveRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if err := assertDirectoryChain(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// profileDir resolves the private target directory for one validated name. Name
// validation already forbids separators and traversal; the relative check is
// defense in depth against a future validation regression.
func profileDir(root, name string) (string, error) {
	if !config.ValidProfileName(name) {
		return "", fmt.Errorf("invalid profile name %q", name)
	}
	base := filepath.Join(root, "profiles")
	dir := filepath.Join(base, name)
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel != name {
		return "", errors.New("profile path escapes the configuration root")
	}
	return dir, nil
}

// assertDirectoryChain rejects symlinked or non-directory path components.
func assertDirectoryChain(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(abs, current), current) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is not a safe directory", current)
		}
	}
	return nil
}

// confirm reads a plain y/n answer. Empty input or end of input aborts.
func confirm(stdin io.Reader) bool {
	if stdin == nil {
		return false
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// readPrivateFile reads a bounded regular file without following symlinks.
func readPrivateFile(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return data, nil
}

// writePrivateFile creates a new owner-only file, refusing to overwrite.
func writePrivateFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
