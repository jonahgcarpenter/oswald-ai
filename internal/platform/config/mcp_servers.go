package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// MCPServerSpec carries optional MCP server fields for AddMCPServer and
// UpdateMCPServer. Nil pointers and nil maps mean untouched (edit); Add
// requires exactly one transport. String removal lists delete map keys.
type MCPServerSpec struct {
	URL           *string
	Command       *string
	Args          *[]string
	Cwd           *string
	EnvSet        map[string]string
	EnvRemove     []string
	Transport     *string
	HeadersSet    map[string]string
	HeadersRemove []string
	Include       *[]string
	Exclude       *[]string
	// Auth is "oauth" to enable OAuth, "" or "none" to clear auth and the
	// OAuth block. Any OAuth* field also enables OAuth unless Auth says none.
	Auth              *string
	OAuthClientID     *string
	OAuthClientSecret *string
	OAuthFlow         *string
	OAuthScopes       *[]string
	OAuthTimeout      *string
	// ClearOAuth drops the OAuth block but keeps the auth mode (fully
	// dynamic discovery when auth is oauth).
	ClearOAuth bool
	Enabled    *bool
}

// AddMCPServer inserts a new mcp_servers.<name> entry. The candidate moves
// through the same staged validation as every other config write; nothing
// touches disk on failure.
func AddMCPServer(root, profile, name string, spec MCPServerSpec) error {
	return mutateMCPServer(root, profile, name, false, func(current mcpServerYAML) (mcpServerYAML, error) {
		next, err := applyMCPServerSpec(mcpServerYAML{}, spec)
		if err != nil {
			return mcpServerYAML{}, err
		}
		if strings.TrimSpace(next.URL) == "" && strings.TrimSpace(next.Command) == "" {
			return mcpServerYAML{}, errors.New("server needs exactly one of --url or --command")
		}
		_ = current
		return next, nil
	})
}

// UpdateMCPServer merges spec fields over the existing mcp_servers.<name>
// entry. Unspecified fields keep their values. Transport and auth switches
// clear the other side's keys so the one-of rules keep validating.
func UpdateMCPServer(root, profile, name string, spec MCPServerSpec) error {
	return mutateMCPServer(root, profile, name, true, func(current mcpServerYAML) (mcpServerYAML, error) {
		return applyMCPServerSpec(current, spec)
	})
}

// RemoveMCPServer deletes the mcp_servers.<name> entry, reporting whether it
// existed. Token material under mcp-tokens/ is left alone; use --with-tokens
// handling in the caller to drop it too.
func RemoveMCPServer(root, profile, name string) (bool, error) {
	configPath, err := ConfigPathForProfile(root, profile)
	if err != nil {
		return false, err
	}
	orig, node, err := readManagedDocRaw(configPath, profile)
	if err != nil {
		return false, err
	}
	servers, err := mcpServersNode(node, true)
	if err != nil {
		return false, err
	}
	if servers == nil {
		return false, nil
	}
	removed := removeMappingKey(servers, name)
	if !removed {
		return false, nil
	}
	candidate, err := marshalManaged(node)
	if err != nil {
		return false, err
	}
	if err := StageValidateConfig(root, configPath, candidate); err != nil {
		return false, err
	}
	if len(orig) > 0 {
		if err := SaveLastKnownGood(configPath, orig); err != nil {
			return false, errors.New("configuration file unavailable")
		}
	}
	return true, AtomicWritePrivateFile(configPath, candidate)
}

// mutateMCPServer loads the managed document, applies mutate to the named
// server entry (which must exist iff mustExist), and writes the candidate
// through staged validation with last-known-good tracking.
func mutateMCPServer(root, profile, name string, mustExist bool, mutate func(mcpServerYAML) (mcpServerYAML, error)) error {
	configPath, err := ConfigPathForProfile(root, profile)
	if err != nil {
		return err
	}
	orig, node, err := readManagedDocRaw(configPath, profile)
	if err != nil {
		return err
	}
	servers, err := mcpServersNode(node, true)
	if err != nil {
		return err
	}
	if servers == nil {
		if mustExist {
			return fmt.Errorf("unknown MCP server %q", name)
		}
		servers = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "mcp_servers"}, servers)
	}
	child := mappingManagedChild(servers, name)
	if mustExist && child == nil {
		return fmt.Errorf("unknown MCP server %q", name)
	}
	if !mustExist && child != nil {
		return fmt.Errorf("MCP server %q already exists", name)
	}
	var current mcpServerYAML
	if child != nil {
		if err := child.Decode(&current); err != nil {
			return errors.New("configuration file is not valid YAML")
		}
	}
	next, err := mutate(current)
	if err != nil {
		return err
	}
	replacement := encodeMCPServer(next)
	if child != nil {
		*child = *replacement
	} else {
		servers.Content = append(servers.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, replacement)
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

// mcpServersNode returns the mcp_servers mapping node, or nil when absent.
// Missing documents never occur here: readManagedDocRaw guarantees a mapping.
func mcpServersNode(doc *yaml.Node, _ bool) (*yaml.Node, error) {
	if doc == nil || doc.Kind != yaml.MappingNode {
		return nil, errors.New("configuration file is not valid YAML")
	}
	return mappingManagedChild(doc, "mcp_servers"), nil
}

// removeMappingKey deletes one key/value pair from a mapping node.
func removeMappingKey(node *yaml.Node, key string) bool {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
			return true
		}
	}
	return false
}

// applyMCPServerSpec merges spec over base. Pointer presence (not value)
// decides what changes, so edit can distinguish untouched from cleared.
func applyMCPServerSpec(base mcpServerYAML, spec MCPServerSpec) (mcpServerYAML, error) {
	if spec.URL != nil && spec.Command != nil &&
		strings.TrimSpace(*spec.URL) != "" && strings.TrimSpace(*spec.Command) != "" {
		return mcpServerYAML{}, errors.New("specify exactly one transport: url or command, not both")
	}
	out := base
	if out.Env == nil && (len(spec.EnvSet) > 0 || len(spec.EnvRemove) > 0) {
		out.Env = map[string]string{}
	} else if out.Env != nil {
		cp := make(map[string]string, len(out.Env))
		for k, v := range out.Env {
			cp[k] = v
		}
		out.Env = cp
	}
	if out.Headers == nil && (len(spec.HeadersSet) > 0 || len(spec.HeadersRemove) > 0) {
		out.Headers = map[string]string{}
	} else if out.Headers != nil {
		cp := make(map[string]string, len(out.Headers))
		for k, v := range out.Headers {
			cp[k] = v
		}
		out.Headers = cp
	}
	if spec.URL != nil {
		if strings.TrimSpace(*spec.URL) == "" {
			out.URL = ""
		} else {
			out.URL = strings.TrimSpace(*spec.URL)
			out.Command, out.Args, out.Cwd = "", nil, ""
			out.Env = nil
		}
	}
	if spec.Command != nil {
		if strings.TrimSpace(*spec.Command) == "" {
			out.Command, out.Args, out.Cwd = "", nil, ""
			out.Env = nil
		} else {
			out.Command = strings.TrimSpace(*spec.Command)
			out.URL, out.Transport, out.Auth = "", "", ""
			out.Headers = nil
			out.OAuth = mcpOAuthYAML{}
		}
	}
	if spec.Args != nil {
		out.Args = append([]string(nil), *spec.Args...)
	}
	if spec.Cwd != nil {
		out.Cwd = strings.TrimSpace(*spec.Cwd)
	}
	for k, v := range spec.EnvSet {
		out.Env[k] = v
	}
	for _, k := range spec.EnvRemove {
		delete(out.Env, k)
	}
	if len(out.Env) == 0 {
		out.Env = nil
	}
	if spec.Transport != nil {
		out.Transport = strings.TrimSpace(*spec.Transport)
	}
	for k, v := range spec.HeadersSet {
		out.Headers[k] = v
	}
	for _, k := range spec.HeadersRemove {
		delete(out.Headers, k)
	}
	if len(out.Headers) == 0 {
		out.Headers = nil
	}
	if spec.Include != nil {
		out.Tools.Include = append([]string(nil), *spec.Include...)
	}
	if spec.Exclude != nil {
		out.Tools.Exclude = append([]string(nil), *spec.Exclude...)
	}
	oauthTouched := spec.OAuthClientID != nil || spec.OAuthClientSecret != nil ||
		spec.OAuthFlow != nil || spec.OAuthScopes != nil || spec.OAuthTimeout != nil
	if spec.Auth != nil {
		switch strings.ToLower(strings.TrimSpace(*spec.Auth)) {
		case "oauth":
			out.Auth = "oauth"
		case "", "none":
			out.Auth = ""
			out.OAuth = mcpOAuthYAML{}
		default:
			return mcpServerYAML{}, fmt.Errorf("auth must be oauth or none")
		}
	}
	if spec.ClearOAuth {
		out.OAuth = mcpOAuthYAML{}
	}
	if spec.OAuthClientID != nil {
		out.OAuth.ClientID = strings.TrimSpace(*spec.OAuthClientID)
	}
	if spec.OAuthClientSecret != nil {
		out.OAuth.ClientSecret = strings.TrimSpace(*spec.OAuthClientSecret)
	}
	if spec.OAuthFlow != nil {
		out.OAuth.Flow = strings.TrimSpace(*spec.OAuthFlow)
	}
	if spec.OAuthScopes != nil {
		out.OAuth.Scopes = append([]string(nil), *spec.OAuthScopes...)
	}
	if spec.OAuthTimeout != nil {
		out.OAuth.Timeout = strings.TrimSpace(*spec.OAuthTimeout)
	}
	if oauthTouched && (spec.Auth == nil || strings.ToLower(strings.TrimSpace(*spec.Auth)) == "oauth") {
		if strings.TrimSpace(out.Auth) == "" {
			out.Auth = "oauth"
		}
	}
	if spec.Enabled != nil {
		enabled := *spec.Enabled
		out.Enabled = &enabled
	}
	return out, nil
}

// encodeMCPServer renders a server entry, omitting zero fields so the
// staged validator sees a clean document.
func encodeMCPServer(server mcpServerYAML) *yaml.Node {
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	put := func(key string, value *yaml.Node) {
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
	}
	str := func(value string) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	}
	if strings.TrimSpace(server.URL) != "" {
		put("url", str(strings.TrimSpace(server.URL)))
	}
	if strings.TrimSpace(server.Command) != "" {
		put("command", str(strings.TrimSpace(server.Command)))
	}
	if len(server.Args) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, arg := range server.Args {
			seq.Content = append(seq.Content, str(arg))
		}
		put("args", seq)
	}
	if strings.TrimSpace(server.Cwd) != "" {
		put("cwd", str(strings.TrimSpace(server.Cwd)))
	}
	if len(server.Env) > 0 {
		put("env", stringMapNode(server.Env))
	}
	if strings.TrimSpace(server.Transport) != "" {
		put("transport", str(strings.TrimSpace(server.Transport)))
	}
	if len(server.Headers) > 0 {
		put("headers", stringMapNode(server.Headers))
	}
	if len(server.Tools.Include) > 0 || len(server.Tools.Exclude) > 0 {
		tools := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		if len(server.Tools.Include) > 0 {
			seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			for _, name := range server.Tools.Include {
				seq.Content = append(seq.Content, str(name))
			}
			tools.Content = append(tools.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "include"}, seq)
		}
		if len(server.Tools.Exclude) > 0 {
			seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			for _, name := range server.Tools.Exclude {
				seq.Content = append(seq.Content, str(name))
			}
			tools.Content = append(tools.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "exclude"}, seq)
		}
		put("tools", tools)
	}
	if strings.TrimSpace(server.Auth) != "" {
		put("auth", str(strings.TrimSpace(server.Auth)))
	}
	oauth := server.OAuth
	if strings.TrimSpace(oauth.ClientID) != "" || strings.TrimSpace(oauth.ClientSecret) != "" ||
		strings.TrimSpace(oauth.Flow) != "" || len(oauth.Scopes) > 0 || strings.TrimSpace(oauth.Timeout) != "" {
		block := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		if strings.TrimSpace(oauth.ClientID) != "" {
			block.Content = append(block.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "client_id"}, str(strings.TrimSpace(oauth.ClientID)))
		}
		if strings.TrimSpace(oauth.ClientSecret) != "" {
			block.Content = append(block.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "client_secret"}, str(strings.TrimSpace(oauth.ClientSecret)))
		}
		if strings.TrimSpace(oauth.Flow) != "" {
			block.Content = append(block.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "flow"}, str(strings.TrimSpace(oauth.Flow)))
		}
		if len(oauth.Scopes) > 0 {
			seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			for _, scope := range oauth.Scopes {
				seq.Content = append(seq.Content, str(scope))
			}
			block.Content = append(block.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "scopes"}, seq)
		}
		if strings.TrimSpace(oauth.Timeout) != "" {
			block.Content = append(block.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "timeout"}, str(strings.TrimSpace(oauth.Timeout)))
		}
		put("oauth", block)
	}
	if server.Enabled != nil {
		put("enabled", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(*server.Enabled)})
	}
	return node
}

// stringMapNode renders a string map with sorted keys for stable output.
func stringMapNode(values map[string]string) *yaml.Node {
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: values[key]})
	}
	return node
}

// mcpTokenDirForProfile returns the profile token directory without creating
// anything. Missing directories simply mean no tokens were ever stored.
func mcpTokenDirForProfile(profileRoot, server string) (string, error) {
	abs, err := filepath.Abs(profileRoot)
	if err != nil {
		return "", errors.New("invalid configuration root")
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", errors.New("invalid configuration root")
	}
	return filepath.Join(abs, "mcp-tokens", server), nil
}
