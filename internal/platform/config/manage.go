package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// MaxModelConfigRunes bounds redacted configuration text handed to the model.
// The full document stays on disk; narrower paths read without truncation.
const MaxModelConfigRunes = 8000

// envNameRE bounds dotenv variable names.
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SplitConfigPath parses a dot-separated schema path into segments.
func SplitConfigPath(path string) ([]string, error) {
	var segments []string
	for _, segment := range strings.Split(path, ".") {
		if segment = strings.TrimSpace(segment); segment != "" {
			segments = append(segments, segment)
		}
	}
	if len(segments) == 0 {
		return nil, errors.New("path is required")
	}
	return segments, nil
}

// ConfigPathForProfile returns the config.yaml owned by one profile: the root
// file for default, the provisioned subtree file for named profiles.
func ConfigPathForProfile(root, profile string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", errors.New("invalid configuration root")
	}
	if profile == "" || profile == "default" {
		return filepath.Join(abs, "config.yaml"), nil
	}
	if !ValidProfileName(profile) {
		return "", fmt.Errorf("invalid profile name %q", profile)
	}
	dir := filepath.Join(abs, "profiles", profile)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("unknown profile %q", profile)
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// EnvPathForProfile returns the .env owned by one profile.
func EnvPathForProfile(root, profile string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", errors.New("invalid configuration root")
	}
	if profile == "" || profile == "default" {
		return filepath.Join(abs, ".env"), nil
	}
	if !ValidProfileName(profile) {
		return "", fmt.Errorf("invalid profile name %q", profile)
	}
	dir := filepath.Join(abs, "profiles", profile)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("unknown profile %q", profile)
	}
	return filepath.Join(dir, ".env"), nil
}

// GlobalRootForProfile resolves the configuration root owning a profile root:
// the root itself for default, its grandparent for named subtrees.
func GlobalRootForProfile(profileRoot, profile string) (string, error) {
	abs, err := filepath.Abs(profileRoot)
	if err != nil {
		return "", errors.New("invalid configuration root")
	}
	if profile == "" || profile == "default" {
		return abs, nil
	}
	if filepath.Base(filepath.Dir(abs)) != "profiles" {
		return "", errors.New("invalid configuration root")
	}
	return filepath.Dir(filepath.Dir(abs)), nil
}

// InlineConfigPath reports whether an agent may write a config leaf without
// operator approval: the hot, recoverable, low-blast-radius subset (log level
// plus admission allow/ban/mention rules). Everything else is approval-track.
func InlineConfigPath(segments []string) bool {
	if len(segments) == 0 {
		return false
	}
	if segments[0] == "runtime" {
		return true
	}
	if segments[0] == "platforms" && len(segments) >= 4 && segments[len(segments)-2] == "extra" {
		switch segments[len(segments)-1] {
		case "allowed_users", "banned_users", "require_mention":
			return true
		}
	}
	return false
}

// SensitiveConfigPath reports whether a config leaf needs operator approval
// for agent writes: anything outside the inline subset, including every
// secret-bearing leaf and the gateway profile mapping.
func SensitiveConfigPath(segments []string) bool { return !InlineConfigPath(segments) }

// ShowConfigFile returns redacted configuration text: the whole document or
// the subtree at sub, truncated to MaxModelConfigRunes.
func ShowConfigFile(configPath, profile string, sub []string) (string, error) {
	node, err := readManagedDoc(configPath, profile)
	if err != nil {
		return "", err
	}
	if len(sub) > 0 {
		node, err = navigateManaged(node, sub)
		if err != nil {
			return "", err
		}
	}
	return TruncateModelText(encodeManagedRedacted(node, sub)), nil
}

// SetConfigValue validates and atomically writes one leaf path. The candidate
// is staged through Validate before anything touches disk; the previous bytes
// are kept as the undo target.
func SetConfigValue(root, profile string, segments []string, raw string) error {
	if len(segments) == 0 {
		return errors.New("path is required")
	}
	configPath, err := ConfigPathForProfile(root, profile)
	if err != nil {
		return err
	}
	orig, node, err := readManagedDocRaw(configPath, profile)
	if err != nil {
		return err
	}
	if err := assignManaged(node, segments, parseManagedValue(raw)); err != nil {
		return err
	}
	candidate, err := marshalManaged(node)
	if err != nil {
		return err
	}
	if err := StageValidateConfig(root, configPath, candidate); err != nil {
		return err
	}
	if len(orig) > 0 {
		if err := SaveLastKnownGood(configPath, orig); err != nil {
			return errors.New("configuration file unavailable")
		}
	}
	return AtomicWritePrivateFile(configPath, candidate)
}

// UnsetConfigValue clears one leaf path with the same safe write semantics.
func UnsetConfigValue(root, profile string, segments []string) error {
	if len(segments) == 0 {
		return errors.New("path is required")
	}
	configPath, err := ConfigPathForProfile(root, profile)
	if err != nil {
		return err
	}
	orig, node, err := readManagedDocRaw(configPath, profile)
	if err != nil {
		return err
	}
	if err := removeManaged(node, segments); err != nil {
		return err
	}
	candidate, err := marshalManaged(node)
	if err != nil {
		return err
	}
	if err := StageValidateConfig(root, configPath, candidate); err != nil {
		return err
	}
	if len(orig) > 0 {
		if err := SaveLastKnownGood(configPath, orig); err != nil {
			return errors.New("configuration file unavailable")
		}
	}
	return AtomicWritePrivateFile(configPath, candidate)
}

// DiffConfigText returns redacted current vs last-known-good text, or
// "no changes\n" when they match.
func DiffConfigText(configPath string) (string, error) {
	current, err := readManagedBounded(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			current = []byte("{}\n")
		} else {
			return "", err
		}
	}
	previous, err := ReadLastKnownGood(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "no changes\n", nil
		}
		return "", errors.New("configuration file unavailable")
	}
	currentRedacted, previousRedacted := redactManagedBytes(current), redactManagedBytes(previous)
	if bytes.Equal(currentRedacted, previousRedacted) {
		return "no changes\n", nil
	}
	return lineManagedDiff(previousRedacted, currentRedacted), nil
}

// UndoConfigValue restores the last-known-good bytes after validating them.
// The undone state becomes the new undo target so undo is reversible.
func UndoConfigValue(root, profile string) error {
	configPath, err := ConfigPathForProfile(root, profile)
	if err != nil {
		return err
	}
	previous, err := ReadLastKnownGood(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("nothing to undo")
		}
		return errors.New("configuration file unavailable")
	}
	if err := StageValidateConfig(root, configPath, previous); err != nil {
		return err
	}
	current, err := readManagedBounded(configPath)
	if err != nil {
		return err
	}
	if err := AtomicWritePrivateFile(configPath, previous); err != nil {
		return err
	}
	if err := SaveLastKnownGood(configPath, current); err != nil {
		return errors.New("configuration file unavailable")
	}
	return nil
}

// ConfigChangeSensitive reports whether the flattened leaf diff between two
// documents touches any approval-track path. It gates undo for the agent.
func ConfigChangeSensitive(current, previous []byte) bool {
	oldLeaves := flattenManaged(previous)
	newLeaves := flattenManaged(current)
	for path, oldValue := range oldLeaves {
		if newValue, ok := newLeaves[path]; !ok || !reflect.DeepEqual(oldValue, newValue) {
			if SensitiveConfigPath(strings.Split(path, ".")) {
				return true
			}
		}
	}
	for path := range newLeaves {
		if _, ok := oldLeaves[path]; !ok {
			if SensitiveConfigPath(strings.Split(path, ".")) {
				return true
			}
		}
	}
	return false
}

// ListEnvNames returns sorted dotenv variable names, never values.
func ListEnvNames(envPath string) ([]string, error) {
	values, err := readManagedEnv(envPath)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// SetEnvValue validates and atomically writes one dotenv variable.
func SetEnvValue(root, profile, name, value string) error {
	if !envNameRE.MatchString(name) {
		return fmt.Errorf("invalid variable name %q", name)
	}
	envPath, err := EnvPathForProfile(root, profile)
	if err != nil {
		return err
	}
	orig, values, err := readManagedEnvRaw(envPath)
	if err != nil {
		return err
	}
	values[name] = value
	candidate, err := godotenv.Marshal(values)
	if err != nil {
		return errors.New("environment file is not valid")
	}
	if !strings.HasSuffix(candidate, "\n") {
		candidate += "\n"
	}
	if err := StageValidateEnv(root, envPath, []byte(candidate)); err != nil {
		return err
	}
	if orig != nil {
		if err := SaveLastKnownGood(envPath, orig); err != nil {
			return errors.New("environment file unavailable")
		}
	}
	return AtomicWritePrivateFile(envPath, []byte(candidate))
}

// UnsetEnvValue removes one dotenv variable with the same safe semantics.
func UnsetEnvValue(root, profile, name string) error {
	if !envNameRE.MatchString(name) {
		return fmt.Errorf("invalid variable name %q", name)
	}
	envPath, err := EnvPathForProfile(root, profile)
	if err != nil {
		return err
	}
	orig, values, err := readManagedEnvRaw(envPath)
	if err != nil {
		return err
	}
	if _, ok := values[name]; !ok {
		return fmt.Errorf("unknown variable %q", name)
	}
	delete(values, name)
	candidate, err := godotenv.Marshal(values)
	if err != nil {
		return errors.New("environment file is not valid")
	}
	if candidate != "" && !strings.HasSuffix(candidate, "\n") {
		candidate += "\n"
	}
	if err := StageValidateEnv(root, envPath, []byte(candidate)); err != nil {
		return err
	}
	if orig != nil {
		if err := SaveLastKnownGood(envPath, orig); err != nil {
			return errors.New("environment file unavailable")
		}
	}
	return AtomicWritePrivateFile(envPath, []byte(candidate))
}

// TruncateModelText bounds model-facing text with an explicit marker.
func TruncateModelText(text string) string {
	if len([]rune(text)) <= MaxModelConfigRunes {
		return text
	}
	runes := []rune(text)[:MaxModelConfigRunes]
	return string(runes) + "\n... (truncated; narrow with a schema path)\n"
}

// StageValidateConfig copies the root into a temp staging directory,
// substitutes the candidate for the target file, and runs the exact startup
// pipeline against the staging copy. Live files are untouched on failure.
func StageValidateConfig(root, targetPath string, candidate []byte) error {
	return stageValidate(root, targetPath, candidate, false)
}

// StageValidateEnv stages a candidate .env the same way.
func StageValidateEnv(root, targetPath string, candidate []byte) error {
	if len(candidate) > MaxConfigBytes {
		return errors.New("environment file exceeds size limit")
	}
	if _, err := godotenv.Unmarshal(string(candidate)); len(candidate) > 0 && err != nil {
		return errors.New("environment file is not valid")
	}
	return stageValidate(root, targetPath, candidate, true)
}

func stageValidate(root, targetPath string, candidate []byte, isEnv bool) error {
	if len(candidate) > MaxConfigBytes {
		return errors.New("configuration file exceeds size limit")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return errors.New("configuration file unavailable")
	}
	target, err := filepath.Abs(targetPath)
	if err != nil {
		return errors.New("configuration file unavailable")
	}
	staging, err := os.MkdirTemp("", "oswald-config-*")
	if err != nil {
		return errors.New("configuration file unavailable")
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := os.Chmod(staging, 0700); err != nil {
		return errors.New("configuration file unavailable")
	}
	put := func(src, dst string) error {
		if src == target {
			return os.WriteFile(dst, candidate, 0600)
		}
		data, err := os.ReadFile(src)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return errors.New("configuration file unavailable")
		}
		if len(data) > MaxConfigBytes {
			return errors.New("configuration file exceeds size limit")
		}
		return os.WriteFile(dst, data, 0600)
	}
	if err := put(filepath.Join(abs, "config.yaml"), filepath.Join(staging, "config.yaml")); err != nil {
		return err
	}
	if err := put(filepath.Join(abs, ".env"), filepath.Join(staging, ".env")); err != nil {
		return err
	}
	entries, _ := os.ReadDir(filepath.Join(abs, "profiles"))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dstDir := filepath.Join(staging, "profiles", entry.Name())
		if err := os.MkdirAll(dstDir, 0700); err != nil {
			return errors.New("configuration file unavailable")
		}
		if err := put(filepath.Join(abs, "profiles", entry.Name(), "config.yaml"), filepath.Join(dstDir, "config.yaml")); err != nil {
			return err
		}
		// An empty staged profile config validates as an empty override
		// document, matching startup's optional read.
		if _, err := os.Stat(filepath.Join(dstDir, "config.yaml")); os.IsNotExist(err) {
			if err := os.WriteFile(filepath.Join(dstDir, "config.yaml"), []byte("{}\n"), 0600); err != nil {
				return errors.New("configuration file unavailable")
			}
		}
		if err := put(filepath.Join(abs, "profiles", entry.Name(), ".env"), filepath.Join(dstDir, ".env")); err != nil {
			return err
		}
	}
	// A named profile target without a directory yet stages its candidate alone.
	if rel, err := filepath.Rel(abs, target); err == nil {
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) == 3 && parts[0] == "profiles" {
			dstDir := filepath.Join(staging, "profiles", parts[1])
			if _, err := os.Stat(dstDir); os.IsNotExist(err) {
				if err := os.MkdirAll(dstDir, 0700); err != nil {
					return errors.New("configuration file unavailable")
				}
				name := parts[2]
				if name != "config.yaml" && name != ".env" {
					return errors.New("configuration file unavailable")
				}
				if err := os.WriteFile(filepath.Join(dstDir, name), candidate, 0600); err != nil {
					return errors.New("configuration file unavailable")
				}
			}
		}
	}
	_, err = Validate(staging)
	return err
}

// readManagedDoc reads a config.yaml without following symlinks. A missing
// named profile file reads as an empty document.
func readManagedDoc(configPath, profile string) (*yaml.Node, error) {
	_, node, err := readManagedDocRaw(configPath, profile)
	return node, err
}

func readManagedDocRaw(configPath, profile string) ([]byte, *yaml.Node, error) {
	data, err := readManagedBounded(configPath)
	if err != nil {
		if os.IsNotExist(err) && profile != "default" {
			return []byte("{}\n"), &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
		}
		return nil, nil, err
	}
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&doc); err != nil {
		return nil, nil, errors.New("configuration file is not valid YAML")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("configuration file is not valid YAML")
	}
	return data, doc.Content[0], nil
}

func readManagedBounded(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("configuration file unavailable")
	}
	if info.Size() > MaxConfigBytes {
		return nil, errors.New("configuration file exceeds size limit")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("configuration file unavailable")
	}
	if len(data) > MaxConfigBytes {
		return nil, errors.New("configuration file exceeds size limit")
	}
	return data, nil
}

func readManagedEnv(envPath string) (map[string]string, error) {
	_, values, err := readManagedEnvRaw(envPath)
	return values, err
}

func readManagedEnvRaw(envPath string) ([]byte, map[string]string, error) {
	data, err := os.ReadFile(envPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, map[string]string{}, nil
		}
		return nil, nil, errors.New("environment file unavailable")
	}
	if len(data) > MaxConfigBytes {
		return nil, nil, errors.New("environment file exceeds size limit")
	}
	if len(data) == 0 {
		return data, map[string]string{}, nil
	}
	values, err := godotenv.Unmarshal(string(data))
	if err != nil {
		return nil, nil, errors.New("environment file is not valid")
	}
	return data, values, nil
}

func navigateManaged(node *yaml.Node, segments []string) (*yaml.Node, error) {
	current := node
	for _, segment := range segments {
		if current == nil || current.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("unknown path %q", strings.Join(segments, "."))
		}
		next := mappingManagedChild(current, segment)
		if next == nil {
			return nil, fmt.Errorf("unknown path %q", strings.Join(segments, "."))
		}
		current = next
	}
	return current, nil
}

func assignManaged(node *yaml.Node, segments []string, value *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("unknown path %q", strings.Join(segments, "."))
	}
	current := node
	for _, segment := range segments[:len(segments)-1] {
		next := mappingManagedChild(current, segment)
		if next == nil {
			key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: segment}
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			current.Content = append(current.Content, key, next)
		} else if next.Kind != yaml.MappingNode {
			return fmt.Errorf("unknown path %q", strings.Join(segments, "."))
		}
		current = next
	}
	leaf := segments[len(segments)-1]
	for i := 0; i+1 < len(current.Content); i += 2 {
		if current.Content[i].Value == leaf {
			current.Content[i+1] = value
			return nil
		}
	}
	key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: leaf}
	current.Content = append(current.Content, key, value)
	return nil
}

func removeManaged(node *yaml.Node, segments []string) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("unknown path %q", strings.Join(segments, "."))
	}
	if len(segments) == 1 {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == segments[0] {
				node.Content = append(node.Content[:i], node.Content[i+2:]...)
				return nil
			}
		}
		return fmt.Errorf("unknown path %q", strings.Join(segments, "."))
	}
	current, err := navigateManaged(node, segments[:len(segments)-1])
	if err != nil {
		return err
	}
	if current.Kind != yaml.MappingNode {
		return fmt.Errorf("unknown path %q", strings.Join(segments, "."))
	}
	leaf := segments[len(segments)-1]
	for i := 0; i+1 < len(current.Content); i += 2 {
		if current.Content[i].Value == leaf {
			current.Content = append(current.Content[:i], current.Content[i+2:]...)
			return nil
		}
	}
	return fmt.Errorf("unknown path %q", strings.Join(segments, "."))
}

func mappingManagedChild(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// parseManagedValue decodes raw input as YAML so bools, ints, lists, and maps
// keep their types; unparseable input stays a plain string.
func parseManagedValue(raw string) *yaml.Node {
	var doc yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	if err := decoder.Decode(&doc); err != nil || len(doc.Content) != 1 {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: raw}
	}
	value := doc.Content[0]
	if value.Kind == yaml.ScalarNode && value.Tag == "!!str" {
		value.Value = raw
		return value
	}
	if value.Kind == yaml.ScalarNode && value.Tag != "!!bool" && value.Tag != "!!int" && value.Tag != "!!float" && value.Tag != "!!null" {
		value.Value = raw
		value.Tag = "!!str"
		return value
	}
	return value
}

func marshalManaged(node *yaml.Node) ([]byte, error) {
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{node}}
	if err := encoder.Encode(doc); err != nil {
		return nil, errors.New("configuration file is not valid YAML")
	}
	if err := encoder.Close(); err != nil {
		return nil, errors.New("configuration file is not valid YAML")
	}
	if out.Len() > MaxConfigBytes {
		return nil, errors.New("configuration file exceeds size limit")
	}
	return out.Bytes(), nil
}

// flattenManaged projects a document to dotted leaf paths for diffing.
func flattenManaged(data []byte) map[string]interface{} {
	var value interface{}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&value); err != nil {
		return map[string]interface{}{}
	}
	out := map[string]interface{}{}
	var walk func(prefix string, node interface{})
	walk = func(prefix string, node interface{}) {
		switch typed := node.(type) {
		case map[string]interface{}:
			for key, child := range typed {
				path := key
				if prefix != "" {
					path = prefix + "." + key
				}
				walk(path, child)
			}
		case []interface{}:
			for i, child := range typed {
				walk(fmt.Sprintf("%s.%d", prefix, i), child)
			}
		default:
			out[prefix] = node
		}
	}
	walk("", value)
	return out
}

// managedSecretKeys names leaf keys whose values are never printed. Matching
// is case-insensitive on the final path segment; maps known to carry
// credentials redact every leaf beneath them.
var managedSecretKeys = map[string]bool{
	"key": true, "brave_key": true, "token": true, "server_password": true,
	"client_secret": true, "client_id": true, "password": true, "secret": true,
	"authorization": true,
}

var managedSecretMaps = map[string]bool{
	"headers": true, "env": true, "oauth": true,
}

const managedRedactedValue = "[redacted]"

// encodeManagedRedacted marshals a node with secret leaves replaced.
func encodeManagedRedacted(node *yaml.Node, path []string) string {
	redacted := redactManagedCopy(node, path)
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{redacted}}
	if err := encoder.Encode(doc); err != nil {
		return "configuration unavailable\n"
	}
	_ = encoder.Close()
	return out.String()
}

// redactManagedBytes redacts secret leaves in raw file bytes. Unparseable
// input redacts to a fixed message so values never leak through a failure.
func redactManagedBytes(data []byte) []byte {
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&doc); err != nil || len(doc.Content) != 1 {
		return []byte("configuration unavailable\n")
	}
	return []byte(encodeManagedRedacted(doc.Content[0], nil))
}

func redactManagedCopy(node *yaml.Node, path []string) *yaml.Node {
	if node == nil {
		return nil
	}
	copy := *node
	copy.Content = nil
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			childPath := append(append([]string{}, path...), key.Value)
			copy.Content = append(copy.Content, key, redactManagedCopy(node.Content[i+1], childPath))
		}
		return &copy
	}
	if node.Kind == yaml.SequenceNode {
		for _, child := range node.Content {
			copy.Content = append(copy.Content, redactManagedCopy(child, path))
		}
		return &copy
	}
	if node.Kind == yaml.ScalarNode && isManagedSecret(path) {
		copy.Value = managedRedactedValue
		copy.Tag = "!!str"
	}
	return &copy
}

func isManagedSecret(path []string) bool {
	if len(path) == 0 {
		return false
	}
	for _, segment := range path {
		if managedSecretMaps[strings.ToLower(segment)] {
			return true
		}
	}
	return managedSecretKeys[strings.ToLower(path[len(path)-1])]
}

// lineManagedDiff renders previous vs current with common prefix/suffix elided.
func lineManagedDiff(previous, current []byte) string {
	prevLines := strings.Split(string(previous), "\n")
	curLines := strings.Split(string(current), "\n")
	head := 0
	for head < len(prevLines) && head < len(curLines) && prevLines[head] == curLines[head] {
		head++
	}
	tail := 0
	for tail < len(prevLines)-head && tail < len(curLines)-head && prevLines[len(prevLines)-1-tail] == curLines[len(curLines)-1-tail] {
		tail++
	}
	var out strings.Builder
	for _, line := range prevLines[head : len(prevLines)-tail] {
		out.WriteString("- " + line + "\n")
	}
	for _, line := range curLines[head : len(curLines)-tail] {
		out.WriteString("+ " + line + "\n")
	}
	return out.String()
}
