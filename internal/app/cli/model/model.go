// Package model implements the `oswald model` command group: explicit,
// operator-driven reads and writes of the top-level model section. All
// mutation goes through the mediated core (staged validation before write,
// last-known-good, atomic replace).
package model

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/app/cli/help"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/chatgpt"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/claude"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

// Exit codes mirror the parent cli convention without importing it.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// Leaf usage lines double as `--help` pages: `oswald model <command> --help`
// prints the same text to stdout and exits 0 without resolving scope.
const (
	showUsage = "usage: oswald model show [--profile NAME]"
	listUsage = "usage: oswald model list [--profile NAME]"
	setUsage  = "usage: oswald model set [--provider P] [--default M] [--context-length N] [--profile NAME]"
)

// listFetchTimeout and listFetchConcurrency bound one `model list` run: at
// most four providers in flight, each with its own deadline, so one dead
// endpoint cannot stall the whole listing.
const (
	listFetchTimeout     = 8 * time.Second
	listFetchConcurrency = 4
)

// Run dispatches the model command group.
func Run(ctx context.Context, root string, args []string, stdout, stderr io.Writer) int {
	profile, rest := splitProfile(args)
	if len(rest) == 0 {
		modelUsage(stderr)
		return exitUsage
	}
	if len(rest) >= 2 && help.WantsHelp(rest[1:]) {
		switch rest[0] {
		case "show":
			fmt.Fprintln(stdout, showUsage)
			return exitOK
		case "list":
			fmt.Fprintln(stdout, listUsage)
			return exitOK
		case "set":
			fmt.Fprintln(stdout, setUsage)
			return exitOK
		}
	}
	target, err := resolveTarget(root, profile, !(rest[0] == "show" || rest[0] == "list"))
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	switch rest[0] {
	case "show":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, showUsage)
			return exitUsage
		}
		return show(target, stdout, stderr)
	case "list":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, listUsage)
			return exitUsage
		}
		return listCmd(ctx, target, stdout, stderr)
	case "set":
		return setCmd(target, rest[1:], stdout, stderr)
	case "help", "-h", "--help":
		modelUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown model command %q\n", rest[0])
		modelUsage(stderr)
		return exitUsage
	}
}

func modelUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald model [--profile NAME] <show|list|set> [options]")
	fmt.Fprintln(w, "  show                       model section")
	fmt.Fprintln(w, "  list                       live models per provider")
	fmt.Fprintln(w, "  set --provider P           set model.provider (custom:<name>)")
	fmt.Fprintln(w, "  set --default M            set model.default")
	fmt.Fprintln(w, "  set --context-length N     set model.context_length")
}

type target struct {
	root    string
	profile string
	path    string
}

func resolveTarget(root, profile string, mutating bool) (*target, error) {
	var name string
	var err error
	if mutating {
		name, err = config.ResolveProfile(profile)
	} else {
		name, err = config.ResolveReadProfile(profile)
	}
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("invalid configuration root")
	}
	path, err := config.ConfigPathForProfile(abs, name)
	if err != nil {
		return nil, err
	}
	return &target{root: abs, profile: name, path: path}, nil
}

func show(t *target, stdout, stderr io.Writer) int {
	out, err := config.ShowConfigFile(t.path, t.profile, []string{"model"})
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprint(stdout, out)
	return exitOK
}

// listCmd prints the acting profile's providers and their live model
// listings, one provider per block. It is strictly read-only and never fails
// on an unavailable provider: the listing is informational, so only scope and
// configuration problems are errors.
func listCmd(ctx context.Context, t *target, stdout, stderr io.Writer) int {
	cfg, err := config.Validate(t.root)
	if err != nil {
		reportModelError(stderr, err)
		return exitError
	}
	active := cfg
	if t.profile != "default" {
		active = cfg.Profiles[t.profile]
		if active == nil {
			reportModelError(stderr, &config.ConfigError{Code: "config_profile_unavailable", Source: "profile_config"})
			return exitError
		}
	}

	names := make([]string, 0, len(active.ProviderEndpoints))
	for name := range active.ProviderEndpoints {
		names = append(names, name)
	}
	sort.Strings(names)

	entries := make([]providerModels, len(names))
	var wg sync.WaitGroup
	permits := make(chan struct{}, listFetchConcurrency)
	for index, name := range names {
		index, name := index, name
		endpoint := active.ProviderEndpoints[name]
		wg.Add(1)
		go func() {
			defer wg.Done()
			permits <- struct{}{}
			defer func() { <-permits }()
			fetchCtx, cancel := context.WithTimeout(ctx, listFetchTimeout)
			defer cancel()
			models, err := subscriptionModels(fetchCtx, active, name, endpoint)
			entries[index] = providerModels{name: name, models: models, unavailable: err != nil}
		}()
	}
	wg.Wait()

	for _, entry := range entries {
		if entry.unavailable {
			fmt.Fprintf(stdout, "%s — unavailable\n", entry.name)
			continue
		}
		fmt.Fprintf(stdout, "%s — %d models\n", entry.name, len(entry.models))
		for _, model := range entry.models {
			if model.Window > 0 {
				fmt.Fprintf(stdout, "    %s (context %d)\n", model.ID, model.Window)
				continue
			}
			fmt.Fprintf(stdout, "    %s\n", model.ID)
		}
	}
	return exitOK
}

// providerModels is one provider's fetched listing. Unavailable records a
// fetch failure without retaining the underlying error, which never needs to
// reach the operator.
type providerModels struct {
	name        string
	models      []llm.ModelEntry
	unavailable bool
}

// subscriptionModels resolves one provider's listing: unauthorized ChatGPT
// subscriptions degrade instead of advertising models that cannot run; key
// providers use the live gateway listing.
func subscriptionModels(ctx context.Context, active *config.Config, name string, endpoint config.ProviderEndpoint) ([]llm.ModelEntry, error) {
	switch endpoint.AuthKind {
	case config.ProviderAuthChatGPT:
		client, err := chatgpt.NewClient(endpoint.URL, active.ProfileRoot, name, active.LLMGatewayVirtualKey, nil, nil)
		if err != nil {
			return nil, err
		}
		return client.ListModels(ctx)
	case config.ProviderAuthClaudeSDK:
		if _, err := claude.Check(ctx); err != nil {
			return nil, err
		}
		return claude.StaticModels(), nil
	default:
		return llm.NewGatewayClient(endpoint.URL, endpoint.APIKey, active.LLMGatewayVirtualKey, nil).ListModels(ctx)
	}
}

func setCmd(t *target, args []string, stdout, stderr io.Writer) int {
	sub := flag.NewFlagSet("model set", flag.ContinueOnError)
	sub.SetOutput(stderr)
	provider := sub.String("provider", "", "model provider (custom:<name>)")
	def := sub.String("default", "", "default model id")
	contextLength := sub.String("context-length", "", "context window size")
	flagArgs, rest := partitionSetArgs(args)
	if err := sub.Parse(flagArgs); err != nil {
		return exitUsage
	}
	if len(rest) != 0 {
		fmt.Fprintln(stderr, setUsage)
		return exitUsage
	}
	set := 0
	if *provider != "" {
		set++
	}
	if *def != "" {
		set++
	}
	if *contextLength != "" {
		set++
	}
	if set == 0 {
		fmt.Fprintln(stderr, setUsage)
		return exitUsage
	}
	if set > 1 {
		fmt.Fprintln(stderr, "oswald: set one model field per invocation")
		return exitUsage
	}
	oldCfg, _ := config.Validate(t.root)
	var err error
	switch {
	case *provider != "":
		err = config.SetConfigValue(t.root, t.profile, []string{"model", "provider"}, *provider)
	case *def != "":
		err = config.SetConfigValue(t.root, t.profile, []string{"model", "default"}, *def)
	case *contextLength != "":
		err = config.SetConfigValue(t.root, t.profile, []string{"model", "context_length"}, *contextLength)
	}
	if err != nil {
		reportModelError(stderr, err)
		return exitError
	}
	fmt.Fprintf(stdout, "set model (%s)\n", effectForChange(t.root, oldCfg))
	return exitOK
}

func effectForChange(root string, oldCfg *config.Config) string {
	if oldCfg != nil {
		if nextCfg, err := config.Validate(root); err == nil {
			if config.ReloadEffect(oldCfg, nextCfg) == "live" {
				return "applied live"
			}
		}
	}
	return "restart required - run `oswald restart` to apply"
}

// reportModelError prints a classified diagnostic without values.
func reportModelError(stderr io.Writer, err error) {
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

// partitionSetArgs splits tokens so flags are accepted in any position
// relative to the --profile flag.
func partitionSetArgs(args []string) (flagArgs, rest []string) {
	valueFlags := map[string]bool{"profile": true, "provider": true, "default": true, "context-length": true}
	for i := 0; i < len(args); i++ {
		token := args[i]
		name := token
		if j := strings.Index(token, "="); strings.HasPrefix(token, "-") && j >= 0 {
			name = token[:j]
		}
		key := strings.TrimLeft(name, "-")
		if valueFlags[key] {
			flagArgs = append(flagArgs, token)
			if !strings.Contains(token, "=") {
				if i+1 < len(args) {
					i++
					flagArgs = append(flagArgs, args[i])
				}
			}
			continue
		}
		if strings.HasPrefix(token, "-") {
			flagArgs = append(flagArgs, token)
			continue
		}
		rest = append(rest, token)
	}
	return flagArgs, rest
}

// splitProfile extracts --profile NAME / --profile=NAME in any position,
// returning the profile value and remaining args.
func splitProfile(args []string) (string, []string) {
	var rest []string
	profile := ""
	for i := 0; i < len(args); i++ {
		token := args[i]
		if token == "--profile" {
			if i+1 < len(args) {
				i++
				profile = args[i]
			}
			continue
		}
		if strings.HasPrefix(token, "--profile=") {
			profile = strings.TrimPrefix(token, "--profile=")
			continue
		}
		rest = append(rest, token)
	}
	return profile, rest
}
