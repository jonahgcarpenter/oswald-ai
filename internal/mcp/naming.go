package mcp

import (
	"fmt"
	"strings"
)

// bridgeToolPrefix marks deferred MCP tools invoked through the tool_call
// bridge. The full name is mcp__<server>__<remote>.
const bridgeToolPrefix = "mcp__"

const bridgeToolSeparator = "__"

// bridgeToolName qualifies a remote tool for model-visible deferred invocation.
func bridgeToolName(server, remote string) string {
	return bridgeToolPrefix + server + bridgeToolSeparator + remote
}

// parseBridgeToolName splits a deferred tool name into server and remote parts.
// It reports false for names outside the bridge namespace, including legacy
// dot-qualified names which are no longer resolvable.
func parseBridgeToolName(name string) (string, string, bool) {
	name = strings.TrimSpace(name)
	rest, ok := strings.CutPrefix(name, bridgeToolPrefix)
	if !ok {
		return "", "", false
	}
	server, remote, ok := strings.Cut(rest, bridgeToolSeparator)
	if !ok || server == "" || remote == "" {
		return "", "", false
	}
	return server, remote, true
}

// isBridgeToolName reports whether name belongs to the deferred MCP namespace.
func isBridgeToolName(name string) bool {
	_, _, ok := parseBridgeToolName(name)
	return ok
}

// splitToolName resolves a model-visible MCP tool reference. Bridge names
// split on the first separator; legacy dot-qualified names still parse so
// stored history keeps a stable server attribution, but they never resolve
// against the live catalog.
func splitToolName(name string) (string, string, bool) {
	if server, remote, ok := parseBridgeToolName(name); ok {
		return server, remote, true
	}
	server, remote, ok := strings.Cut(strings.TrimSpace(name), ".")
	if !ok || server == "" || remote == "" {
		return "", "", false
	}
	return server, remote, true
}

func qualifiedBridgeNameLength(server, remote string) error {
	if len(bridgeToolName(server, remote)) > maxProviderIdentifierBytes {
		return fmt.Errorf("qualified tool name exceeds %d bytes", maxProviderIdentifierBytes)
	}
	return nil
}
