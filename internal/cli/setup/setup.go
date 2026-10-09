// Package setup implements the `oswald setup` command: seed a first-install
// configuration root. It creates missing files only and never overwrites
// operator content. A later interactive configurator edits the same root.
package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// Run seeds the selected configuration root.
func Run(ctx context.Context, root string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: oswald setup")
		return exitUsage
	}
	results, err := seed(ctx, root)
	if err != nil {
		fmt.Fprintf(stderr, "setup failed: %v\n", err)
		return exitError
	}
	displayRoot := root
	if displayRoot == "" {
		displayRoot = "."
	}
	for _, result := range results {
		verb := "kept"
		if result.created {
			verb = "created"
		}
		fmt.Fprintf(stdout, "%s %s\n", verb, filepath.Join(displayRoot, result.name))
		if result.insecure {
			fmt.Fprintf(stderr, "warning: %s has group/other permissions; run `chmod 600 %s`\n", filepath.Join(displayRoot, result.name), filepath.Join(displayRoot, result.name))
		}
	}
	fmt.Fprintf(stdout, "setup complete: edit %s and %s, then run `oswald start`\n", filepath.Join(displayRoot, "config.yaml"), filepath.Join(displayRoot, ".env"))
	return exitOK
}

type seedResult struct {
	name     string
	created  bool
	insecure bool
}

// seed provisions the root directory, the profiles container, and the default
// files at their required permissions.
func seed(ctx context.Context, root string) ([]seedResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := assertSeedPath(abs); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	if err := requireDirectory(abs); err != nil {
		return nil, err
	}
	profilesDir := filepath.Join(abs, "profiles")
	if err := os.Mkdir(profilesDir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := requireDirectory(profilesDir); err != nil {
		return nil, err
	}
	files := []struct{ name, content string }{
		{"config.yaml", defaultConfig},
		{".env", defaultEnv},
		{"SOUL.md", defaultSoul},
	}
	results := make([]seedResult, 0, len(files))
	for _, file := range files {
		path := filepath.Join(abs, file.name)
		created, err := writeNewPrivateFile(path, []byte(file.content))
		if err != nil {
			return nil, err
		}
		result := seedResult{name: file.name, created: created}
		if !created {
			if info, statErr := os.Lstat(path); statErr == nil && info.Mode().IsRegular() && info.Mode().Perm()&0077 != 0 {
				result.insecure = true
			}
		}
		results = append(results, result)
	}
	return results, nil
}

// assertSeedPath rejects symlinked parents and a symlinked or non-directory
// existing root before anything is created.
func assertSeedPath(abs string) error {
	parent := filepath.Dir(abs)
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(parent, current), current) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is not a safe directory", current)
		}
	}
	if info, err := os.Lstat(abs); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is not a safe directory", abs)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func requireDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a directory", path)
	}
	return nil
}

// writeNewPrivateFile creates an owner-only file, reporting false when the file
// already exists so operator content is never overwritten.
func writeNewPrivateFile(path string, data []byte) (bool, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return false, err
	}
	return true, f.Close()
}

// defaultConfig is the complete supported configuration surface, seeded by
// `oswald setup`. The loopback API runs on the default profile; disabled
// platforms are pruned before interpolation, so this loads with no credentials.
// Replace the model provider before starting.
const defaultConfig = `# Oswald configuration. The oswald setup command writes this file; ${VAR} reads this
# root's private .env and the process environment takes precedence. Named
# profiles inherit the default .env and override it with their own. Disabled
# platforms never require their credentials; unknown keys are still rejected.
providers:
  gateway_name:
    api: http://127.0.0.1:8080/v1
    # key: ${PROVIDER_API_KEY}
model:
  provider: custom:gateway_name
  default: your/provider-model
  context_length: 32768
runtime:
  log_level: info
gateway:
  multiplex_profiles: true
  profile_routes: []
  # Add explicit routes for named profiles; admitted identities without a route
  # fall back to the default profile. Unmapped transport identities are admitted
  # per the platform allow/ban policy below.
  # - name: example-discord
  #   platform: discord
  #   user_id: "123456789"
  #   profile: example
platforms:
  api:
    enabled: true
    extra:
      api_host: 127.0.0.1
      api_port: 8000
  discord:
    enabled: false
    extra:
      token: ${DISCORD_TOKEN:-}
      allowed_users: []   # Explicit allow list; empty admits every routed user.
      banned_users: []    # Users to silently ignore everywhere.
      require_mention: true  # Guild messages must begin with a bot mention.
  bluebubbles:
    enabled: false
    extra:
      server_url: http://127.0.0.1:1234
      server_password: ${BLUEBUBBLES_PASSWORD:-}
      webhook_host: 0.0.0.0
      webhook_port: 8080
      webhook_path: /bluebubbles/webhook
      allowed_users: []
      banned_users: []
      require_mention: true  # Group messages must begin with a mention.
      # mention_pattern: ['@?Oswald\b[,:\-]?']  # Go regexp, anchored at start.
tools:
  web_search:
    # brave_key: ${BRAVE_API_KEY}
    searxng_url: ""
  image_generate:
    url: ""
    generation_timeout: 2m
# MCP connections are isolated by profile and opened lazily; restart to reload.
mcp:
  servers: {}
  # servers:
  #   example:
  #     url: https://example.com/mcp
  #     description: Describe what this server can do.
  #     headers:
  #       Authorization: Bearer ${EXAMPLE_MCP_TOKEN}
`

// defaultEnv is a private placeholder credential file. godotenv can parse it as
// written; commented keys document the expected variable names.
const defaultEnv = `# Private operator credentials. The process environment takes precedence.
# Prefer ${VAR} references in config.yaml over editing this file directly.
# Keep this file private (chmod 600).
#
# PROVIDER_API_KEY=
# DISCORD_TOKEN=
# BLUEBUBBLES_PASSWORD=
# BRAVE_API_KEY=
`

// defaultSoul is the starting operator policy for a new installation.
const defaultSoul = `You are Oswald, built by Jonah Carpenter. Be direct: match the length of your reply to the weight of the ask — a one-line question gets a one-line answer, and finished work gets a short report of what changed, what's verified, and what's left, never a replay of the process. No filler ("Great question," "I'd be happy to"), no restating the request back, no re-summarizing what you already said, no narrating tool calls the user can see. Plain claims over adjectives; when unsure, say so plainly. Agree because it's right, not because the user said it. Depth is earned — give it when the user asks for detail, teaches, or the stakes demand it, not by default.`
