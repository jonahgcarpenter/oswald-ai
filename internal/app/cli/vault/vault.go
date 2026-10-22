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
	fmt.Fprintln(w, "  add --origin URL [--label NAME] [--kind login|address|payment]")
	fmt.Fprintln(w, "      [--identifier-type email|phone|username] [--otp SEED] [--field key=value ...]")
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
	origin := fs.String("origin", "", "exact site origin the item is bound to (e.g. https://example.com)")
	label := fs.String("label", "", "short site name (default: the origin host)")
	kind := fs.String("kind", "login", "item kind: login, address, or payment")
	idType := fs.String("identifier-type", "email", "login identifier type: email, phone, or username")
	otp := fs.String("otp", "", "optional authenticator seed or otpauth://totp URI for 2FA codes")
	fields := fieldFlags{}
	fs.Var(fields, "field", "address/payment field as key=value (repeatable)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if strings.TrimSpace(*origin) == "" {
		fmt.Fprintln(stderr, "oswald: vault add requires --origin")
		return exitUsage
	}
	switch *kind {
	case "login":
		return addLogin(vs, *origin, *label, *idType, *otp, stdin, stdout, stderr)
	case "address", "payment":
		return addCheckout(vs, *kind, *origin, *label, fields, stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "oswald: unknown vault kind %q\n", *kind)
		return exitUsage
	}
}

// fieldFlags collects repeatable --field key=value options.
type fieldFlags map[string]string

func (f fieldFlags) String() string { return fmt.Sprintf("%v", map[string]string(f)) }

func (f fieldFlags) Set(raw string) error {
	name, value, ok := strings.Cut(raw, "=")
	if !ok || strings.TrimSpace(name) == "" {
		return errors.New("field must look like key=value")
	}
	f[strings.TrimSpace(name)] = strings.TrimSpace(value)
	return nil
}

func addLogin(vs *store.Store, origin, label, idType, otp string, stdin io.Reader, stdout, stderr io.Writer) int {
	switch idType {
	case "email", "phone", "username":
	default:
		fmt.Fprintf(stderr, "oswald: unknown identifier type %q\n", idType)
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
	name := strings.TrimSpace(label)
	if name == "" {
		name = strings.TrimSpace(origin)
	}
	meta, err := vs.AddLogin(name, idType, strings.TrimSpace(identifier), password, strings.TrimSpace(otp), strings.TrimSpace(origin))
	password = ""
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "saved %s %s for %s (handle %s)\n", meta.Kind, meta.Label, meta.Origin, meta.ID)
	return exitOK
}

func addCheckout(vs *store.Store, kind, origin, label string, fields fieldFlags, stdin io.Reader, stdout, stderr io.Writer) int {
	allowed := store.AddressFields
	if kind == store.KindPayment {
		allowed = store.PaymentFields
		fmt.Fprintln(stdout, "Note: payment cards are stored for listing only; the browser never fills them.")
	}
	collected := map[string]string{}
	for k, v := range fields {
		if _, ok := allowed[k]; ok && v != "" {
			collected[k] = v
		}
	}
	input := bufio.NewReader(stdin)
	for _, key := range requiredKeys(kind) {
		if collected[key] != "" {
			continue
		}
		fmt.Fprintf(stdout, "%s:\n", key)
		var (
			value string
			err   error
		)
		if kind == store.KindPayment {
			value, err = readSecret(stdin, input, stdout)
		} else {
			value, err = readLine(input)
		}
		if err != nil || strings.TrimSpace(value) == "" {
			fmt.Fprintf(stderr, "oswald: %s is required\n", key)
			return exitError
		}
		collected[key] = strings.TrimSpace(value)
	}
	name := strings.TrimSpace(label)
	if name == "" {
		name = strings.TrimSpace(origin)
	}
	var (
		meta store.VaultItemMeta
		err  error
	)
	if kind == store.KindPayment {
		meta, err = vs.AddPayment(name, collected, strings.TrimSpace(origin))
	} else {
		meta, err = vs.AddAddress(name, collected, strings.TrimSpace(origin))
	}
	for k := range collected {
		collected[k] = ""
	}
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "saved %s %s for %s (handle %s)\n", meta.Kind, meta.Label, meta.Origin, meta.ID)
	return exitOK
}

// requiredKeys returns required field names in stable order.
func requiredKeys(kind string) []string {
	if kind == store.KindPayment {
		return []string{"card_number", "exp_month", "exp_year", "cvc"}
	}
	return []string{"address_line1", "city", "postal_code", "country"}
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
