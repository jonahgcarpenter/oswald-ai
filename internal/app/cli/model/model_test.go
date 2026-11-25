package model

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testConfig = `providers:
  fake:
    api: http://127.0.0.1:9999/v1
  other:
    api: http://127.0.0.1:9998/v1
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
platforms:
  api:
    enabled: true
    extra:
      api_port: 12345
runtime:
  log_level: info
`

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func run(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), root, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestShowDisplaysModel(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, _ := run(t, root, "show")
	if code != 0 || !strings.Contains(out, "fake/model") {
		t.Fatalf("show = %d %q", code, out)
	}
}

func TestSetDefaultRoundTrip(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, stderr := run(t, root, "set", "--default", "fake/other")
	if code != 0 {
		t.Fatalf("set = %d stderr=%s", code, stderr)
	}
	if !strings.Contains(out, "restart required - run `oswald restart` to apply") {
		t.Fatalf("set missing applying hint = %q", out)
	}
	code, out, _ = run(t, root, "show")
	if code != 0 || !strings.Contains(out, "fake/other") {
		t.Fatalf("show after set = %d %q", code, out)
	}
}

func TestSetProviderSwitch(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, stderr := run(t, root, "set", "--provider", "custom:other"); code != 0 {
		t.Fatalf("provider switch rejected: %s", stderr)
	}
}

func TestSetRejectsUnknownProvider(t *testing.T) {
	root := writeConfig(t, testConfig)
	before, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	code, _, _ := run(t, root, "set", "--provider", "custom:missing")
	if code == 0 {
		t.Fatal("unknown provider accepted")
	}
	after, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if !bytes.Equal(before, after) {
		t.Fatal("file mutated on failed validation")
	}
}

func TestSetRejectsMultipleFields(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, _ := run(t, root, "set", "--default", "a", "--provider", "custom:fake"); code != exitUsage {
		t.Fatalf("multi-field set = %d, want usage", code)
	}
}

func TestCrossProfileReadDeniedMutateDenied(t *testing.T) {
	root := writeConfig(t, testConfig)
	if err := os.MkdirAll(filepath.Join(root, "profiles", "bob"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "profiles", "bob", "config.yaml"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSWALD_HOME", filepath.Join(root, "profiles", "bob"))
	if code, _, _ := run(t, root, "show", "--profile", "default"); code == 0 {
		t.Fatal("cross-profile read accepted")
	}
	before, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if code, _, _ := run(t, root, "set", "--profile", "default", "--default", "fake/evil"); code == 0 {
		t.Fatal("cross-profile set accepted")
	}
	after, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if !bytes.Equal(before, after) {
		t.Fatal("default config mutated by rejected set")
	}
}

func TestHelpPages(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		code, out, _ := run(t, root, args...)
		if code != exitOK || !strings.Contains(out, "usage: oswald model") {
			t.Fatalf("%s %v = %d %q, want exit 0 with group usage", "model", args, code, out)
		}
	}
	for _, leaf := range []string{"show", "list", "set"} {
		code, out, _ := run(t, root, leaf, "--help")
		if code != exitOK || !strings.Contains(out, "usage: oswald model "+leaf) {
			t.Fatalf("model %s --help = %d %q, want exit 0 with leaf usage", leaf, code, out)
		}
	}
}

// modelsServer serves one OpenAI-compatible models listing and records the
// Authorization header the client sent. Fixtures are synthetic throughout.
func modelsServer(t *testing.T, body string, authHeaders chan<- string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if authHeaders != nil {
			select {
			case authHeaders <- r.Header.Get("Authorization"):
			default:
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// closedEndpoint reserves then releases a loopback port so a fetch fails with
// an immediate connection refused instead of hanging.
func closedEndpoint(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return "http://" + addr + "/v1"
}

func listConfig(liveAPI, deadAPI string) string {
	return fmt.Sprintf(`providers:
  live:
    api: %q
    key: synthetic-live-key
  dead:
    api: %q
model:
  provider: custom:live
  default: live/example-model
  context_length: 32768
platforms:
  api:
    enabled: true
    extra:
      api_port: 12345
runtime:
  log_level: info
`, liveAPI, deadAPI)
}

func TestListProvidersLiveAndUnavailable(t *testing.T) {
	authHeaders := make(chan string, 1)
	server := modelsServer(t, `{"data":[{"id":"example/model-alpha","context_length":8192},{"id":"example/model-beta"}]}`, authHeaders)
	root := writeConfig(t, listConfig(server.URL+"/v1", closedEndpoint(t)))

	code, out, stderr := run(t, root, "list")
	if code != exitOK {
		t.Fatalf("list = %d stderr=%q", code, stderr)
	}
	want := "dead — unavailable\n" +
		"live — 2 models\n" +
		"    example/model-alpha (context 8192)\n" +
		"    example/model-beta\n"
	if out != want {
		t.Fatalf("list output:\n%s\nwant:\n%s", out, want)
	}
	select {
	case header := <-authHeaders:
		if header != "Bearer synthetic-live-key" {
			t.Fatalf("live provider received %q, want the configured bearer", header)
		}
	default:
		t.Fatal("live provider never received a models request")
	}
}

func TestListScopesToActingProfile(t *testing.T) {
	defaultServer := modelsServer(t, `{"data":[{"id":"example/default-model"}]}`, nil)
	profileServer := modelsServer(t, `{"data":[{"id":"example/profile-model","context_length":16384}]}`, nil)

	root := writeConfig(t, fmt.Sprintf(`providers:
  default-live:
    api: %q
    key: synthetic-default-key
model:
  provider: custom:default-live
  default: default-live/example-model
  context_length: 32768
gateway:
  multiplex_profiles: true
  profile_routes:
    - platform: "*"
      user_id: "*"
      profile: bob
platforms:
  api:
    enabled: true
    extra:
      api_port: 12345
runtime:
  log_level: info
`, defaultServer.URL+"/v1"))
	profileDir := filepath.Join(root, "profiles", "bob")
	if err := os.MkdirAll(profileDir, 0700); err != nil {
		t.Fatal(err)
	}
	profileConfig := fmt.Sprintf(`providers:
  bob-live:
    api: %q
    key: synthetic-bob-key
model:
  provider: custom:bob-live
  default: bob-live/example-model
  context_length: 16384
`, profileServer.URL+"/v1")
	if err := os.WriteFile(filepath.Join(profileDir, "config.yaml"), []byte(profileConfig), 0600); err != nil {
		t.Fatal(err)
	}

	code, out, stderr := run(t, root, "list")
	if code != exitOK {
		t.Fatalf("default list = %d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "example/default-model") {
		t.Fatalf("default listing missing default provider model:\n%s", out)
	}
	if strings.Contains(out, "example/profile-model") {
		t.Fatalf("default listing leaked the named profile's provider:\n%s", out)
	}

	code, out, stderr = run(t, root, "list", "--profile", "bob")
	if code != exitOK {
		t.Fatalf("profile list = %d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "    example/profile-model (context 16384)\n") {
		t.Fatalf("profile listing missing profile provider model:\n%s", out)
	}
}

func TestListCrossProfileDenied(t *testing.T) {
	defaultServer := modelsServer(t, `{"data":[{"id":"example/default-model"}]}`, nil)
	root := writeConfig(t, fmt.Sprintf(`providers:
  default-live:
    api: %q
    key: synthetic-default-key
model:
  provider: custom:default-live
  default: default-live/example-model
  context_length: 32768
gateway:
  multiplex_profiles: true
  profile_routes:
    - platform: "*"
      user_id: "*"
      profile: bob
platforms:
  api:
    enabled: true
    extra:
      api_port: 12345
runtime:
  log_level: info
`, defaultServer.URL+"/v1"))
	profileDir := filepath.Join(root, "profiles", "bob")
	if err := os.MkdirAll(profileDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "config.yaml"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSWALD_HOME", profileDir)
	if code, _, stderr := run(t, root, "list", "--profile", "default"); code != exitError || !strings.Contains(stderr, "oswald: ") {
		t.Fatalf("cross-profile list = %d stderr=%q, want exit 1 with a classified error", code, stderr)
	}
}

func TestListHelpNeedsNoScope(t *testing.T) {
	// A bare temp dir has no configurable root; help must answer first.
	code, out, _ := run(t, t.TempDir(), "list", "--help")
	if code != exitOK || !strings.Contains(out, listUsage) {
		t.Fatalf("list --help = %d %q, want exit 0 with leaf usage", code, out)
	}
}

func TestListRejectsExtraArgs(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, stderr := run(t, root, "list", "extra")
	if code != exitUsage || !strings.Contains(stderr, listUsage) {
		t.Fatalf("list extra = %d stdout=%q stderr=%q, want usage exit", code, out, stderr)
	}
}

func TestListUnknownProfile(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, _, stderr := run(t, root, "list", "--profile", "ghost")
	if code != exitError || !strings.HasPrefix(stderr, "oswald: ") {
		t.Fatalf("list unknown profile = %d stderr=%q, want exit 1 with a classified error", code, stderr)
	}
}
