// Package help owns the shared `--help` convention for the operator CLI:
// `oswald <group> --help` prints the group page, and
// `oswald <group> <command> --help` prints that command's page. Help always
// goes to stdout with exit 0 and never parses or mutates anything.
package help

// WantsHelp reports whether args request the help page: the first token is
// help, -h, or --help. First-position-only so a literal value in a later
// position (for example `oswald env set KEY --help`) can never misfire into
// help output.
func WantsHelp(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "help", "-h", "--help":
		return true
	default:
		return false
	}
}
