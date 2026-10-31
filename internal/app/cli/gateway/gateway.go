// Package gateway implements the `oswald gateway` command group: explicit,
// operator-driven management of the top-level gateway section (profile
// routing). It owns gateway.multiplex_profiles and gateway.profile_routes.
// All mutation goes through the mediated core (staged validation before
// write, last-known-good, atomic replace). This group is global-only.
//
// The old `oswald gateway <platform> <enable|disable>` spelling is retired:
// platform toggles live under `oswald platforms enable|disable`.
package gateway

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"gopkg.in/yaml.v3"
)

// Exit codes mirror the parent cli convention without importing it.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// maxConfigBytes bounds the configuration file, matching the runtime read limit.
const maxConfigBytes = 256 * 1024

// validRoutePlatforms is the exact set of route platforms in config files.
var validRoutePlatforms = map[string]bool{"discord": true, "bluebubbles": true}

// retiredPlatforms catches the old spelling so the error can redirect.
var retiredPlatforms = map[string]bool{"discord": true, "bluebubbles": true, "api": true, "imessage": true}

// Run dispatches the gateway command group.
func Run(_ context.Context, root string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		gatewayUsage(stderr)
		return exitUsage
	}
	// Retired spelling: oswald gateway <platform> <enable|disable>.
	if len(args) == 2 && retiredPlatforms[strings.ToLower(args[0])] &&
		(args[1] == "enable" || args[1] == "disable") {
		fmt.Fprintln(stderr, "oswald: use 'oswald platforms enable|disable <name>' instead")
		gatewayUsage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "show":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: oswald gateway show")
			return exitUsage
		}
		return show(root, stdout, stderr)
	case "set-multiplex":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: oswald gateway set-multiplex <true|false>")
			return exitUsage
		}
		return setMultiplex(root, args[1], stdout, stderr)
	case "routes":
		return routesCmd(root, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		gatewayUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown gateway command %q\n", args[0])
		gatewayUsage(stderr)
		return exitUsage
	}
}

func gatewayUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald gateway <show|set-multiplex|routes> [options]")
	fmt.Fprintln(w, "  show                       gateway routing section")
	fmt.Fprintln(w, "  set-multiplex <BOOL>       set gateway.multiplex_profiles")
	fmt.Fprintln(w, "  routes list                list profile routes")
	fmt.Fprintln(w, "  routes add --name N --platform discord|bluebubbles --user-id ID --profile P")
	fmt.Fprintln(w, "  routes remove <name>      remove a route by name")
	fmt.Fprintln(w, "  (platform toggles moved to: oswald platforms enable|disable <name>)")
}

func globalPath(root string) (string, string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", "", errors.New("invalid configuration root")
	}
	path, err := config.ConfigPathForProfile(abs, "default")
	if err != nil {
		return "", "", err
	}
	return abs, path, nil
}

func show(root string, stdout, stderr io.Writer) int {
	_, path, err := globalPath(root)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	out, err := config.ShowConfigFile(path, "default", []string{"gateway"})
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprint(stdout, out)
	return exitOK
}

func setMultiplex(root, raw string, stdout, stderr io.Writer) int {
	if raw != "true" && raw != "false" {
		fmt.Fprintln(stderr, "usage: oswald gateway set-multiplex <true|false>")
		return exitUsage
	}
	abs, _, err := globalPath(root)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	oldCfg, _ := config.Validate(abs)
	if err := config.SetConfigValue(abs, "default", []string{"gateway", "multiplex_profiles"}, raw); err != nil {
		reportGatewayError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "set gateway.multiplex_profiles (%s)\n", effectForChange(abs, oldCfg))
	return exitOK
}

func routesCmd(root string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: oswald gateway routes <list|add|remove> [options]")
		return exitUsage
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: oswald gateway routes list")
			return exitUsage
		}
		_, path, err := globalPath(root)
		if err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
		out, err := config.ShowConfigFile(path, "default", []string{"gateway", "profile_routes"})
		if err != nil {
			fmt.Fprintf(stderr, "oswald: %v\n", err)
			return exitError
		}
		fmt.Fprint(stdout, out)
		return exitOK
	case "add":
		return routesAdd(root, args[1:], stdout, stderr)
	case "remove":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: oswald gateway routes remove <name>")
			return exitUsage
		}
		return routesRemove(root, args[1], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, "usage: oswald gateway routes <list|add|remove> [options]")
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown gateway routes command %q\n", args[0])
		return exitUsage
	}
}

func routesAdd(root string, args []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("gateway routes add", flag.ContinueOnError)
	sub.SetOutput(stderr)
	name := sub.String("name", "", "route name")
	platform := sub.String("platform", "", "discord|bluebubbles")
	userID := sub.String("user-id", "", "transport identity")
	profile := sub.String("profile", "", "operator profile")
	flagArgs, rest := partitionRouteArgs(args)
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 0 || *name == "" || *platform == "" || *userID == "" || *profile == "" {
		fmt.Fprintln(stderr, "usage: oswald gateway routes add --name N --platform discord|bluebubbles --user-id ID --profile P")
		return exitUsage
	}
	if !validRoutePlatforms[*platform] {
		fmt.Fprintf(stderr, "oswald: unknown route platform %q\n", *platform)
		return exitUsage
	}
	abs, _, err := globalPath(root)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	oldCfg, _ := config.Validate(abs)
	if err := addRoute(abs, *name, *platform, *userID, *profile); err != nil {
		reportGatewayError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "added route %s (%s)\n", *name, effectForChange(abs, oldCfg))
	return exitOK
}

func routesRemove(root, name string, stdout, stderr io.Writer) int {
	abs, _, err := globalPath(root)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	oldCfg, _ := config.Validate(abs)
	removed, err := removeRoute(abs, name)
	if err != nil {
		reportGatewayError(stderr, err)
		return exitError
	}
	if !removed {
		fmt.Fprintf(stderr, "oswald: unknown route %q\n", name)
		return exitError
	}
	fmt.Fprintf(stdout, "removed route %s (%s)\n", name, effectForChange(abs, oldCfg))
	return exitOK
}

// partitionRouteArgs forwards flags so the route fields accept any order.
func partitionRouteArgs(args []string) (flagArgs, rest []string) {
	for i := 0; i < len(args); i++ {
		token := args[i]
		if !strings.HasPrefix(token, "-") {
			rest = append(rest, token)
			continue
		}
		flagArgs = append(flagArgs, token)
		if strings.Contains(token, "=") {
			continue
		}
		if i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return flagArgs, rest
}

// addRoute appends a profile_routes entry through staged validation.
func addRoute(root, name, platform, userID, profile string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return errors.New("invalid configuration root")
	}
	path := filepath.Join(abs, "config.yaml")
	data, err := readConfigFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&doc); err != nil {
		return errors.New("configuration file is not valid YAML")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return errors.New("configuration file is not valid YAML")
	}
	rootMap := doc.Content[0]
	gateway := mappingValue(rootMap, "gateway")
	if gateway == nil || gateway.Kind != yaml.MappingNode {
		return errors.New("configuration file has no gateway section")
	}
	routesKey := -1
	for i := 0; i+1 < len(gateway.Content); i += 2 {
		if gateway.Content[i].Value == "profile_routes" {
			routesKey = i + 1
			break
		}
	}
	var routes *yaml.Node
	if routesKey < 0 {
		routes = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		gateway.Content = append(gateway.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "profile_routes"}, routes)
	} else {
		routes = gateway.Content[routesKey]
		if routes.Kind != yaml.SequenceNode {
			return errors.New("configuration file has no gateway section")
		}
	}
	for _, item := range routes.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		if mappingValue(item, "name") != nil && mappingValue(item, "name").Value == name {
			return fmt.Errorf("route %q already exists", name)
		}
	}
	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	put := func(k, v string) {
		entry.Content = append(entry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v})
	}
	put("name", name)
	put("platform", platform)
	put("user_id", userID)
	put("profile", profile)
	routes.Content = append(routes.Content, entry)
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return errors.New("configuration file is not valid YAML")
	}
	if err := encoder.Close(); err != nil {
		return errors.New("configuration file is not valid YAML")
	}
	if out.Len() > maxConfigBytes {
		return errors.New("configuration file exceeds size limit")
	}
	return config.CommitConfigCandidate(abs, path, out.Bytes(), data)
}

// removeRoute deletes a profile_routes entry by name.
func removeRoute(root, name string) (bool, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return false, errors.New("invalid configuration root")
	}
	path := filepath.Join(abs, "config.yaml")
	data, err := readConfigFile(path)
	if err != nil {
		return false, err
	}
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&doc); err != nil {
		return false, errors.New("configuration file is not valid YAML")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return false, errors.New("configuration file is not valid YAML")
	}
	rootMap := doc.Content[0]
	gateway := mappingValue(rootMap, "gateway")
	if gateway == nil || gateway.Kind != yaml.MappingNode {
		return false, errors.New("configuration file has no gateway section")
	}
	var routes *yaml.Node
	for i := 0; i+1 < len(gateway.Content); i += 2 {
		if gateway.Content[i].Value == "profile_routes" {
			routes = gateway.Content[i+1]
			break
		}
	}
	if routes == nil || routes.Kind != yaml.SequenceNode {
		return false, nil
	}
	kept := routes.Content[:0:0]
	removed := false
	for _, item := range routes.Content {
		if item.Kind == yaml.MappingNode {
			if n := mappingValue(item, "name"); n != nil && n.Value == name {
				removed = true
				continue
			}
		}
		kept = append(kept, item)
	}
	if !removed {
		return false, nil
	}
	routes.Content = kept
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return false, errors.New("configuration file is not valid YAML")
	}
	if err := encoder.Close(); err != nil {
		return false, errors.New("configuration file is not valid YAML")
	}
	if out.Len() > maxConfigBytes {
		return false, errors.New("configuration file exceeds size limit")
	}
	if err := config.CommitConfigCandidate(abs, path, out.Bytes(), data); err != nil {
		return false, err
	}
	return true, nil
}

func effectForChange(root string, oldCfg *config.Config) string {
	if oldCfg != nil {
		if nextCfg, err := config.Validate(root); err == nil {
			if config.ReloadEffect(oldCfg, nextCfg) == "live" {
				return "applied live"
			}
		}
	}
	return "restart required to take effect"
}

// reportGatewayError prints a classified diagnostic without values.
func reportGatewayError(stderr io.Writer, err error) {
	if cerr, ok := config.AsConfigError(err); ok {
		location := cerr.Path
		if location == "" {
			location = cerr.Source
		}
		if location != "" {
			fmt.Fprintf(stderr, "oswald: invalid configuration: %s at %s\n", cerr.Code, location)
			return
		}
		fmt.Fprintf(stderr, "oswald: invalid configuration: %s\n", cerr.Code)
		return
	}
	fmt.Fprintf(stderr, "oswald: %v\n", err)
}

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// readConfigFile reads the configuration file without following symlinks.
func readConfigFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("configuration file not found")
		}
		return nil, errors.New("configuration file unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("configuration file unavailable")
	}
	if info.Size() > maxConfigBytes {
		return nil, errors.New("configuration file exceeds size limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil || len(data) > maxConfigBytes {
		return nil, errors.New("configuration file unavailable")
	}
	return data, nil
}
