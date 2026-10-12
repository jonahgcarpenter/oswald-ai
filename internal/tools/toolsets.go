// Package tools owns builtin composition; toolsets name stable bundles for
// exposure decisions. Toolsets are code-owned constants, not operator
// configuration: gateways map to toolsets, and the API intersects them with
// caller opt-in via named/allowed_tools.
package tools

// Toolset names for exposure bundles.
const (
	ToolsetCore          = "core"
	ToolsetTerminal      = "terminal"
	ToolsetCodeExecution = "code_execution"
	ToolsetCronjob       = "cronjob"
)

// coreTools are always-exposed builtins (subject to provider configuration).
var coreTools = []string{
	"memory",
	"session_search",
	"web_search",
	"image_generate",
	"vision_analyze",
	"read_file",
	"write_file",
	"patch",
	"search_files",
}

// ToolsetMembers returns the builtin names in a toolset in stable order.
// Unknown toolsets return nil.
func ToolsetMembers(name string) []string {
	switch name {
	case ToolsetCore:
		return append([]string(nil), coreTools...)
	case ToolsetTerminal:
		return []string{"terminal", "process_manage"}
	case ToolsetCodeExecution:
		return []string{"execute_code"}
	case ToolsetCronjob:
		return []string{"cronjob_manage"}
	default:
		return nil
	}
}

// GatewayToolsets returns the toolsets exposed on a gateway. The API
// gateway (openai) receives core only; caller opt-in adds the rest by name.
func GatewayToolsets(gateway string) []string {
	switch gateway {
	case "openai":
		return []string{ToolsetCore}
	default:
		return []string{ToolsetCore, ToolsetTerminal, ToolsetCodeExecution, ToolsetCronjob}
	}
}
