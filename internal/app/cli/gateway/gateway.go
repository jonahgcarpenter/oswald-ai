// Package gateway implements the `oswald gateway` command group: explicit,
// operator-driven toggling of gateway platforms. It flips only the
// platforms.<name>.enabled flag in the global config.yaml.
package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"gopkg.in/yaml.v3"
)

// Exit codes mirror the parent cli convention without importing it.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// maxConfigBytes bounds the configuration file, matching the runtime read limit.
const maxConfigBytes = 256 * 1024

// validGateways is the exact set of platforms.<name> keys this command manages.
var validGateways = map[string]bool{"discord": true, "bluebubbles": true, "api": true}

// Run dispatches the gateway command group.
// Syntax: oswald gateway <name> <enable|disable>.
func Run(_ context.Context, root string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		gatewayUsage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "help", "-h", "--help":
		gatewayUsage(stdout)
		return exitOK
	}
	if len(args) != 2 {
		gatewayUsage(stderr)
		return exitUsage
	}
	name, action := args[0], args[1]
	if !validGateways[name] {
		fmt.Fprintf(stderr, "oswald: unknown gateway %q\n", name)
		gatewayUsage(stderr)
		return exitUsage
	}
	var want bool
	switch action {
	case "enable":
		want = true
	case "disable":
		want = false
	default:
		fmt.Fprintf(stderr, "oswald: unknown gateway action %q\n", action)
		gatewayUsage(stderr)
		return exitUsage
	}
	if err := setEnabled(root, name, want); err != nil {
		if errors.Is(err, errAlready) {
			if want {
				fmt.Fprintf(stdout, "gateway %s already enabled\n", name)
			} else {
				fmt.Fprintf(stdout, "gateway %s already disabled\n", name)
			}
			return exitOK
		}
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	if want {
		fmt.Fprintf(stdout, "gateway %s enabled\n", name)
	} else {
		fmt.Fprintf(stdout, "gateway %s disabled\n", name)
	}
	return exitOK
}

func gatewayUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald gateway <name> <enable|disable>")
}

// errAlready reports the flag is already in the requested state.
var errAlready = errors.New("already in requested state")

// setEnabled flips platforms.<name>.enabled to the requested boolean value.
func setEnabled(root, name string, want bool) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return errors.New("invalid configuration root")
	}
	path := filepath.Join(abs, "config.yaml")
	data, err := readConfigFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&doc); err != nil {
		return errors.New("configuration file is not valid YAML")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return errors.New("configuration file is not valid YAML")
	}
	rootMap := doc.Content[0]
	platforms := mappingValue(rootMap, "platforms")
	if platforms == nil || platforms.Kind != yaml.MappingNode {
		return fmt.Errorf("unknown gateway %q", name)
	}
	entry := mappingValue(platforms, name)
	if entry == nil || entry.Kind != yaml.MappingNode {
		return fmt.Errorf("unknown gateway %q", name)
	}
	enabled := mappingValue(entry, "enabled")
	if enabled == nil || enabled.Kind != yaml.ScalarNode {
		return fmt.Errorf("gateway %q has no enabled setting", name)
	}
	if enabled.Tag == "!!bool" && ((want && enabled.Value == "true") || (!want && enabled.Value == "false")) {
		return errAlready
	}
	enabled.Tag = "!!bool"
	enabled.Style = 0
	if want {
		enabled.Value = "true"
	} else {
		enabled.Value = "false"
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return errors.New("configuration file is not valid YAML")
	}
	if err := encoder.Close(); err != nil {
		return errors.New("configuration file is not valid YAML")
	}
	if out.Len() > maxConfigBytes {
		return errors.New("configuration file exceeds size limit")
	}
	if err := atomicWriteFile(path, out.Bytes()); err != nil {
		return err
	}
	return nil
}

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// readConfigFile reads the configuration file without following symlinks.
func readConfigFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("configuration file not found")
		}
		return nil, errors.New("configuration file unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("configuration file unavailable")
	}
	if info.Size() > maxConfigBytes {
		return nil, errors.New("configuration file exceeds size limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil || len(data) > maxConfigBytes {
		return nil, errors.New("configuration file unavailable")
	}
	return data, nil
}

// atomicWriteFile replaces the configuration file via a temp file and rename.
// The existing file must be a regular file and not a symlink.
func atomicWriteFile(path string, data []byte) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("configuration file unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("configuration file unavailable")
	}
	dir := filepath.Dir(path)
	tmp, err := os.OpenFile(filepath.Join(dir, ".config.yaml.tmp"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New("configuration file is locked")
		}
		return errors.New("configuration file unavailable")
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return errors.New("configuration file unavailable")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("configuration file unavailable")
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return errors.New("configuration file unavailable")
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errors.New("configuration file unavailable")
	}
	tmpName = ""
	return nil
}
