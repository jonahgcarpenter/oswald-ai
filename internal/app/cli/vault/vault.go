// Package vault implements the `oswald vault` command group: explicit,
// operator-driven management of the profile-scoped encrypted credential
// store used by the browser_vault_* tools. Secrets are read from the
// terminal (masked when interactive) and never echoed or logged.
package vault

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	store "github.com/jonahgcarpenter/oswald-ai/internal/vault"
	"golang.org/x/term"
)

// Exit codes mirror the parent cli convention without importing it.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// Run dispatches the vault command group.
func Run(_ context.Context, root string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("vault", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := fs.String("profile", "default", "operator profile owning the vault")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	if len(rest) == 0 {
		vaultUsage(stderr)
		return exitUsage
	}
	storeRoot, err := profileRoot(root, *profile)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	vs := store.NewStore(storeRoot)
	switch rest[0] {
	case "add":
		return add(storeRoot, vs, rest[1:], stdin, stdout, stderr)
	case "list":
		return list(vs, stdout, stderr)
	case "rm":
		return remove(vs, rest[1:], stdout, stderr)
	case "help", "-h", "--help":
		vaultUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown vault command %q\n", rest[0])
		vaultUsage(stderr)
		return exitUsage
	}
}

func vaultUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald vault [--profile NAME] <add|list|rm> [options]")
	fmt.Fprintln(w, "  add --origin URL [--label NAME] [--identifier-type email|phone|username] [--otp SEED]")
	fmt.Fprintln(w, "  list")
	fmt.Fprintln(w, "  rm <handle>")
}

// profileRoot resolves the state directory for one validated operator
// profile: the configuration root itself for default, the provisioned
// subtree for named profiles.
func profileRoot(root, name string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if name == "default" || name == "" {
		return abs, nil
	}
	if !config.ValidProfileName(name) {
		return "", fmt.Errorf("invalid profile name %q", name)
	}
	dir := filepath.Join(abs, "profiles", name)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("unknown profile %q", name)
	}
	return dir, nil
}

func add(storeRoot string, vs *store.Store, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	_ = storeRoot
	fs := flag.NewFlagSet("vault add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	origin := fs.String("origin", "", "exact site origin the login is bound to (e.g. https://example.com)")
	label := fs.String("label", "", "short site name (default: the origin host)")
	idType := fs.String("identifier-type", "email", "login identifier type: email, phone, or username")
	otp := fs.String("otp", "", "optional authenticator seed or otpauth://totp URI for 2FA codes")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if strings.TrimSpace(*origin) == "" {
		fmt.Fprintln(stderr, "oswald: vault add requires --origin")
		return exitUsage
	}
	switch *idType {
	case "email", "phone", "username":
	default:
		fmt.Fprintf(stderr, "oswald: unknown identifier type %q\n", *idType)
		return exitUsage
	}
	fmt.Fprintln(stdout, "Identifier (email/username, shown while typing):")
	input := bufio.NewReader(stdin)
	identifier, err := readLine(input)
	if err != nil || strings.TrimSpace(identifier) == "" {
		fmt.Fprintln(stderr, "oswald: identifier is required")
		return exitError
	}
	fmt.Fprintln(stdout, "Password (hidden):")
	password, err := readSecret(stdin, input, stdout)
	if err != nil || password == "" {
		fmt.Fprintln(stderr, "oswald: password is required")
		return exitError
	}
	name := strings.TrimSpace(*label)
	if name == "" {
		name = strings.TrimSpace(*origin)
	}
	meta, err := vs.AddLogin(name, *idType, strings.TrimSpace(identifier), password, strings.TrimSpace(*otp), strings.TrimSpace(*origin))
	password = ""
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "saved login %s for %s (handle %s)\n", meta.Label, meta.Origin, meta.ID)
	return exitOK
}

func list(vs *store.Store, stdout, stderr io.Writer) int {
	items, err := vs.List()
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	if len(items) == 0 {
		fmt.Fprintln(stdout, "no saved logins")
		return exitOK
	}
	for _, item := range items {
		two := ""
		if item.HasOTP {
			two = " 2fa=auto"
		}
		fmt.Fprintf(stdout, "%s  %s  %s  %s%s\n", item.ID, item.Label, item.Origin, item.Identifier, two)
	}
	return exitOK
}

func remove(vs *store.Store, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		fmt.Fprintln(stderr, "usage: oswald vault rm <handle>")
		return exitUsage
	}
	removed, err := vs.Remove(strings.TrimSpace(args[0]))
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	if !removed {
		fmt.Fprintf(stderr, "oswald: no vault item with handle %q\n", strings.TrimSpace(args[0]))
		return exitError
	}
	fmt.Fprintf(stdout, "removed %s\n", strings.TrimSpace(args[0]))
	return exitOK
}

// readLine reads one visible line from a shared reader.
func readLine(input *bufio.Reader) (string, error) {
	if input == nil {
		return "", errors.New("no input")
	}
	line, err := input.ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readSecret reads one line, masking terminal echo when interactive.
func readSecret(stdin io.Reader, input *bufio.Reader, stdout io.Writer) (string, error) {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		raw, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stdout)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
	return readLine(input)
}
