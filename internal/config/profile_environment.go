package config

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

type profileEnvironment map[string]string

func readPrivateConfig(path string, optional bool) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if optional && errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, configErr("config_file_unavailable", "")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256*1024 {
		return nil, configErr("config_file_unsafe", "")
	}
	if filepath.Base(path) == ".env" && info.Mode().Perm()&0077 != 0 {
		return nil, configErr("config_file_unsafe", "")
	}
	data, err := io.ReadAll(io.LimitReader(f, 256*1024+1))
	if err != nil || len(data) > 256*1024 {
		return nil, configErr("config_file_unavailable", "")
	}
	return data, nil
}

// loadProfileEnvironment reads one private .env file. Named profiles merge the
// default environment first so inherited values resolve without duplication.
func loadProfileEnvironment(root, source string) (profileEnvironment, error) {
	data, err := readPrivateConfig(filepath.Join(root, ".env"), true)
	if err != nil {
		return nil, withConfigSource(err, source)
	}
	values := map[string]string{}
	if len(data) > 0 {
		values, err = godotenv.Unmarshal(string(data))
		if err != nil {
			return nil, withConfigSource(configErr("config_env_invalid", ""), source)
		}
	}
	return profileEnvironment(values), nil
}

// mergeEnvironments overlays a profile's own values over the default profile's.
func mergeEnvironments(base, override profileEnvironment) profileEnvironment {
	if len(base) == 0 {
		return override
	}
	merged := make(profileEnvironment, len(base)+len(override))
	for name, value := range base {
		merged[name] = value
	}
	for name, value := range override {
		merged[name] = value
	}
	return merged
}

func (e profileEnvironment) lookup(name string) (string, bool) {
	if value, ok := os.LookupEnv(name); ok {
		return value, true
	}
	value, ok := e[name]
	return value, ok
}

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (e profileEnvironment) credential(literal *string, name string, required bool) (string, error) {
	if literal == nil {
		return e.secret(name, required)
	}
	if name != "" {
		return "", configVarErr("config_credential_conflict", name, "")
	}
	if required && strings.TrimSpace(*literal) == "" {
		return "", configErr("config_credential_missing", "")
	}
	return *literal, nil
}

func (e profileEnvironment) secret(name string, required bool) (string, error) {
	if name == "" {
		if required {
			return "", configErr("config_credential_missing", "")
		}
		return "", nil
	}
	if !environmentName.MatchString(name) {
		return "", configVarErr("config_credential_invalid", name, "")
	}
	value, _ := e.lookup(name)
	if required && strings.TrimSpace(value) == "" {
		return "", configVarErr("config_credential_missing", name, "")
	}
	return value, nil
}

// interpolate resolves Compose-style references without mutating os.Environ.
// Missing bare references fail closed; $$ produces a literal dollar sign.
func (e profileEnvironment) interpolate(text string) (string, error) {
	return e.expand(text, false)
}

// interpolateOptional resolves references but treats unset variables as empty.
// It decides platform enablement before that platform's subtree is validated.
func (e profileEnvironment) interpolateOptional(text string) (string, error) {
	return e.expand(text, true)
}

func (e profileEnvironment) expand(text string, optional bool) (string, error) {
	var out strings.Builder
	for i := 0; i < len(text); {
		if text[i] != '$' {
			out.WriteByte(text[i])
			i++
			continue
		}
		i++
		if i < len(text) && text[i] == '$' {
			out.WriteByte('$')
			i++
			continue
		}
		if i == len(text) || text[i] != '{' {
			return "", configErr("config_interpolation_invalid", "")
		}
		end := strings.IndexByte(text[i+1:], '}')
		if end < 0 {
			return "", configErr("config_interpolation_invalid", "")
		}
		expression := text[i+1 : i+1+end]
		i += end + 2
		name, op, fallback := expression, "", ""
		if at := strings.IndexAny(expression, ":-?"); at >= 0 {
			name = expression[:at]
			rest := expression[at:]
			for _, candidate := range []string{":-", ":?", "-", "?"} {
				if strings.HasPrefix(rest, candidate) {
					op, fallback = candidate, rest[len(candidate):]
					break
				}
			}
			if op == "" {
				return "", configErr("config_interpolation_unsupported", "")
			}
		}
		if !environmentName.MatchString(name) {
			return "", configErr("config_interpolation_invalid", "")
		}
		value, set := e.lookup(name)
		if op == "" && !set {
			if !optional {
				return "", configVarErr("config_variable_missing", name, "")
			}
			value = ""
		} else {
			missing := !set || (strings.HasPrefix(op, ":") && value == "")
			if missing {
				switch op {
				case "-", ":-":
					value = fallback
				case "?", ":?":
					if !optional {
						return "", configVarErr("config_variable_missing", name, "")
					}
					value = ""
				}
			}
		}
		out.WriteString(value)
		if out.Len() > 256*1024 {
			return "", configErr("config_expansion_oversized", "")
		}
	}
	return out.String(), nil
}

func cloneYAML(node *yaml.Node) *yaml.Node {
	copy := *node
	copy.Content = nil
	for _, child := range node.Content {
		copy.Content = append(copy.Content, cloneYAML(child))
	}
	return &copy
}

func validateYAMLNode(node *yaml.Node, depth int) error {
	if depth > 32 || node.Kind == yaml.AliasNode {
		return configErr("config_yaml_invalid", "")
	}
	if node.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
				return configErr("config_key_invalid", "")
			}
			seen[key.Value] = true
		}
	}
	for _, child := range node.Content {
		if err := validateYAMLNode(child, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func readYAMLNode(path string, optional bool) (*yaml.Node, error) {
	data, err := readPrivateConfig(path, optional)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 && optional {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
	}
	var node yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(&node) != nil {
		return nil, configErr("config_yaml_invalid", "")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, configErr("config_yaml_invalid", "")
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return nil, configErr("config_yaml_invalid", "")
	}
	if err := validateYAMLNode(node.Content[0], 0); err != nil {
		return nil, err
	}
	return node.Content[0], nil
}

func mergeYAML(base, override *yaml.Node) *yaml.Node {
	if base.Kind != yaml.MappingNode || override.Kind != yaml.MappingNode {
		return cloneYAML(override)
	}
	result := cloneYAML(base)
	for i := 0; i < len(override.Content); i += 2 {
		found := false
		for j := 0; j < len(result.Content); j += 2 {
			if result.Content[j].Value == override.Content[i].Value {
				result.Content[j+1] = mergeYAML(result.Content[j+1], override.Content[i+1])
				found = true
				break
			}
		}
		if !found {
			result.Content = append(result.Content, cloneYAML(override.Content[i]), cloneYAML(override.Content[i+1]))
		}
	}
	return result
}

func resolveDocument(node *yaml.Node, env profileEnvironment) (documentYAML, error) {
	var doc documentYAML
	copy := cloneYAML(node)
	var expand func(*yaml.Node, string) error
	expand = func(n *yaml.Node, path string) error {
		if n.Kind == yaml.AliasNode {
			return configErr("config_yaml_invalid", path)
		}
		if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
			value, err := env.interpolate(n.Value)
			if err != nil {
				return withConfigPath(err, path)
			}
			n.Value = value
			// Numeric and boolean configuration fields remain typed even when
			// their YAML source is a quoted environment reference.
			if strings.HasPrefix(path, "platforms.") || (strings.HasPrefix(path, "mcp.servers.") && strings.Count(path, ".") == 3 && strings.HasSuffix(path, ".enabled")) || path == "runtime.worker_pool_size" || path == "model.context_length" {
				key := path[strings.LastIndex(path, ".")+1:]
				switch key {
				case "api_port", "webhook_port", "listen_port", "worker_pool_size", "context_length":
					if _, err := strconv.Atoi(value); err != nil {
						return configErr("config_value_invalid", path)
					}
					n.Tag = "!!int"
					n.Style = 0
				case "enabled", "guild_require_mention", "group_require_mention", "dm_mention":
					if value != "true" && value != "false" {
						return configErr("config_value_invalid", path)
					}
					n.Tag = "!!bool"
					n.Style = 0
				}
			}
		}
		for i, child := range n.Content {
			// Configuration keys are fixed schema labels, not environment data.
			if n.Kind == yaml.MappingNode && i%2 == 0 {
				if strings.Contains(child.Value, "$") {
					return configErr("config_key_invalid", path)
				}
				continue
			}
			childPath := path
			if n.Kind == yaml.MappingNode {
				if childPath != "" {
					childPath += "."
				}
				childPath += n.Content[i-1].Value
			}
			if err := expand(child, childPath); err != nil {
				return err
			}
		}
		return nil
	}
	if err := expand(copy, ""); err != nil {
		return doc, err
	}
	data, err := yaml.Marshal(copy)
	if err != nil || len(data) > 256*1024 {
		return doc, configErr("config_expansion_oversized", "")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return doc, configErr("config_yaml_invalid", "")
	}
	return doc, nil
}

func profileDocument(global, override *yaml.Node) (*yaml.Node, error) {
	base := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i := 0; i < len(global.Content); i += 2 {
		switch global.Content[i].Value {
		case "providers", "model", "tools", "mcp":
			base.Content = append(base.Content, cloneYAML(global.Content[i]), cloneYAML(global.Content[i+1]))
		}
	}
	for i := 0; i < len(override.Content); i += 2 {
		switch override.Content[i].Value {
		case "providers", "model", "tools", "mcp":
		default:
			return nil, configErr("config_profile_override_invalid", override.Content[i].Value)
		}
	}
	return mergeYAML(base, override), nil
}
