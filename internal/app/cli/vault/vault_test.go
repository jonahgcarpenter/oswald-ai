package vault

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVaultAddListRemove(t *testing.T) {
	root := t.TempDir()
	stdin := strings.NewReader("alice@example.com\ns3cret-password\n")
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), root, []string{"add", "--origin", "https://example.com/login", "--label", "Example"}, stdin, &stdout, &stderr); code != 0 {
		t.Fatalf("add exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "saved login") {
		t.Fatalf("add output = %q", stdout.String())
	}
	var listOut, listErr bytes.Buffer
	if code := Run(context.Background(), root, []string{"list"}, nil, &listOut, &listErr); code != 0 {
		t.Fatalf("list exit %d: %s", code, listErr.String())
	}
	if !strings.Contains(listOut.String(), "https://example.com") || !strings.Contains(listOut.String(), "alice@example.com") {
		t.Fatalf("list output = %q", listOut.String())
	}
	if strings.Contains(listOut.String(), "s3cret") {
		t.Fatal("list leaked the password")
	}
	handle := strings.Fields(listOut.String())[0]
	var rmOut, rmErr bytes.Buffer
	if code := Run(context.Background(), root, []string{"rm", handle}, nil, &rmOut, &rmErr); code != 0 {
		t.Fatalf("rm exit %d: %s", code, rmErr.String())
	}
	var emptyOut bytes.Buffer
	if code := Run(context.Background(), root, []string{"list"}, nil, &emptyOut, &bytes.Buffer{}); code != 0 {
		t.Fatalf("second list exit %d", code)
	}
	if !strings.Contains(emptyOut.String(), "no saved logins") {
		t.Fatalf("list after rm = %q", emptyOut.String())
	}
}

func TestVaultAddRequiresOriginAndSecrets(t *testing.T) {
	root := t.TempDir()
	var stderr bytes.Buffer
	if code := Run(context.Background(), root, []string{"add"}, strings.NewReader(""), &bytes.Buffer{}, &stderr); code != 2 {
		t.Fatalf("add without origin exit %d", code)
	}
	var stderr2 bytes.Buffer
	if code := Run(context.Background(), root, []string{"add", "--origin", "https://example.com"}, strings.NewReader("\n"), &bytes.Buffer{}, &stderr2); code != 1 {
		t.Fatalf("add without identifier exit %d", code)
	}
	var stderr3 bytes.Buffer
	if code := Run(context.Background(), root, []string{"unknown"}, nil, &bytes.Buffer{}, &stderr3); code != 2 {
		t.Fatalf("unknown command exit %d", code)
	}
}

func TestVaultNamedProfile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "profiles", "work"), 0700); err != nil {
		t.Fatal(err)
	}
	stdin := strings.NewReader("bob@example.com\npw-for-work\n")
	var stdout bytes.Buffer
	if code := Run(context.Background(), root, []string{"--profile", "work", "add", "--origin", "https://work.example"}, stdin, &stdout, &bytes.Buffer{}); code != 0 {
		t.Fatalf("named add exit %d", code)
	}
	var listOut bytes.Buffer
	if code := Run(context.Background(), root, []string{"--profile", "work", "list"}, nil, &listOut, &bytes.Buffer{}); code != 0 {
		t.Fatalf("named list exit %d", code)
	}
	if !strings.Contains(listOut.String(), "https://work.example") {
		t.Fatalf("named list = %q", listOut.String())
	}
	var stderr bytes.Buffer
	if code := Run(context.Background(), root, []string{"--profile", "nosuch", "list"}, nil, &bytes.Buffer{}, &stderr); code == 0 {
		t.Fatal("unknown profile listed")
	}
}
