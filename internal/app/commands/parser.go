package commands

import "strings"

// Parsed is the structured form of slash-prefixed command input.
type Parsed struct {
	Raw      string
	Name     string
	Args     []string
	ArgsText string
}

// IsAttempt reports whether input is a command attempt.
func IsAttempt(input string) bool {
	return strings.HasPrefix(strings.TrimSpace(input), "/")
}

// Parse extracts the slash command name and arguments from input.
func Parse(input string) (Parsed, bool) {
	raw := strings.TrimSpace(input)
	if !strings.HasPrefix(raw, "/") {
		return Parsed{}, false
	}

	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return Parsed{}, false
	}

	name := strings.TrimPrefix(fields[0], "/")
	args := append([]string(nil), fields[1:]...)
	argsText := ""
	if len(args) > 0 {
		argsText = strings.TrimSpace(raw[len(fields[0]):])
	}

	return Parsed{Raw: raw, Name: name, Args: args, ArgsText: argsText}, true
}
