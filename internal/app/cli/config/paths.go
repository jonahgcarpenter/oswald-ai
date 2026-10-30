package config

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// splitPath parses a dot-separated schema path into segments.
func splitPath(path string) []string {
	var segments []string
	for _, segment := range strings.Split(path, ".") {
		segment = strings.TrimSpace(segment)
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	return segments
}

// navigate resolves segments against a mapping tree.
func navigate(node *yaml.Node, segments []string) (*yaml.Node, error) {
	current := node
	for _, segment := range segments {
		if current == nil || current.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("unknown path %q", strings.Join(segments, "."))
		}
		next := mappingChild(current, segment)
		if next == nil {
			return nil, fmt.Errorf("unknown path %q", strings.Join(segments, "."))
		}
		current = next
	}
	return current, nil
}

// assign sets one leaf path, creating intermediate mappings as needed. Exactly
// one leaf mutates per call so a failed write cannot half-apply.
func assign(node *yaml.Node, segments []string, value *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("unknown path %q", strings.Join(segments, "."))
	}
	current := node
	for _, segment := range segments[:len(segments)-1] {
		next := mappingChild(current, segment)
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

// remove clears one leaf path.
func remove(node *yaml.Node, segments []string) error {
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
	current, err := navigate(node, segments[:len(segments)-1])
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

func mappingChild(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// parseValue decodes raw CLI input as YAML so bools, ints, lists, and maps
// keep their types; unparseable input stays a plain string.
func parseValue(raw string) *yaml.Node {
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
	// Quote strings that only parsed as scalars through flow style.
	if value.Kind == yaml.ScalarNode && (value.Tag == "!!bool" || value.Tag == "!!int" || value.Tag == "!!float" || value.Tag == "!!null") {
		return value
	}
	if value.Kind == yaml.ScalarNode {
		value.Value = raw
		value.Tag = "!!str"
		return value
	}
	return value
}

func marshal(node *yaml.Node) ([]byte, error) {
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{node}}
	if err := encoder.Encode(doc); err != nil {
		return nil, fmt.Errorf("configuration file is not valid YAML")
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("configuration file is not valid YAML")
	}
	if out.Len() > 256*1024 {
		return nil, fmt.Errorf("configuration file exceeds size limit")
	}
	return out.Bytes(), nil
}
