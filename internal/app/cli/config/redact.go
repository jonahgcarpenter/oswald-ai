package config

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// secretKeys names leaf keys whose values are never printed. Matching is
// case-insensitive on the final path segment; maps known to carry credentials
// (headers, env, oauth blocks) redact every leaf beneath them.
var secretKeys = map[string]bool{
	"key": true, "brave_key": true, "token": true, "server_password": true,
	"client_secret": true, "client_id": true, "password": true, "secret": true,
	"authorization": true,
}

var secretMaps = map[string]bool{
	"headers": true, "env": true, "oauth": true,
}

const redactedValue = "[redacted]"

// encodeRedacted marshals a node with secret leaves replaced.
func encodeRedacted(node *yaml.Node, path []string) string {
	redacted := redactCopy(node, path)
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{redacted}}
	if err := encoder.Encode(doc); err != nil {
		return fmt.Sprintf("configuration unavailable\n")
	}
	_ = encoder.Close()
	return out.String()
}

// redactBytes redacts secret leaves in raw file bytes. Unparseable input
// redacts to a fixed message so values never leak through a parse failure.
func redactBytes(data []byte) []byte {
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&doc); err != nil || len(doc.Content) != 1 {
		return []byte("configuration unavailable\n")
	}
	return []byte(encodeRedacted(doc.Content[0], nil))
}

func redactCopy(node *yaml.Node, path []string) *yaml.Node {
	if node == nil {
		return nil
	}
	copy := *node
	copy.Content = nil
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			childPath := append(append([]string{}, path...), key.Value)
			copy.Content = append(copy.Content, key, redactCopy(node.Content[i+1], childPath))
		}
		return &copy
	}
	if node.Kind == yaml.SequenceNode {
		for _, child := range node.Content {
			copy.Content = append(copy.Content, redactCopy(child, path))
		}
		return &copy
	}
	if node.Kind == yaml.ScalarNode && isSecret(path) {
		copy.Value = redactedValue
		copy.Tag = "!!str"
	}
	return &copy
}

func isSecret(path []string) bool {
	if len(path) == 0 {
		return false
	}
	for _, segment := range path {
		if secretMaps[strings.ToLower(segment)] {
			return true
		}
	}
	return secretKeys[strings.ToLower(path[len(path)-1])]
}

// lineDiff renders previous vs current with common prefix/suffix elided.
func lineDiff(previous, current []byte) string {
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
