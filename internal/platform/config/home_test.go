package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInferScopeFromHome(t *testing.T) {
	if _, _, ok := InferScopeFromHome(""); ok {
		t.Fatal("empty home accepted")
	}
	if _, _, ok := InferScopeFromHome(filepath.Join(t.TempDir(), "missing")); ok {
		t.Fatal("missing dir accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := InferScopeFromHome(file); ok {
		t.Fatal("file accepted as home")
	}

	root := t.TempDir()
	global, profile, ok := InferScopeFromHome(root)
	if !ok || profile != "default" {
		t.Fatalf("default scope = %q %q %v", global, profile, ok)
	}
	bob := filepath.Join(root, "profiles", "bob")
	if err := os.MkdirAll(bob, 0700); err != nil {
		t.Fatal(err)
	}
	global, profile, ok = InferScopeFromHome(bob)
	if !ok || profile != "bob" {
		t.Fatalf("named scope = %q %q %v", global, profile, ok)
	}
	want, _ := filepath.Abs(root)
	if global != want {
		t.Fatalf("named global = %q, want %q", global, want)
	}
	if _, _, ok := InferScopeFromHome(filepath.Join(bob, "config.yaml")); ok {
		t.Fatal("file inside profile accepted as home")
	}
}

func TestResolveGlobalRoot(t *testing.T) {
	root := t.TempDir()
	bob := filepath.Join(root, "profiles", "bob")
	if err := os.MkdirAll(bob, 0700); err != nil {
		t.Fatal(err)
	}

	t.Setenv(OswaldHomeEnv, bob)
	global, err := ResolveGlobalRoot("")
	if err != nil {
		t.Fatalf("env root rejected: %v", err)
	}
	if want, _ := filepath.Abs(root); global != want {
		t.Fatalf("env global = %q, want %q", global, want)
	}
	if _, err := ResolveGlobalRoot(filepath.Join(t.TempDir())); err == nil {
		t.Fatal("mismatched --root accepted")
	}
	if got, err := ResolveGlobalRoot(root); err != nil || got != root {
		t.Fatalf("matching --root rejected: %q %v", got, err)
	}

	t.Setenv(OswaldHomeEnv, "")
	if got, err := ResolveGlobalRoot(""); err != nil || got != OswaldHomeDir {
		t.Fatalf("default root = %q %v", got, err)
	}
}

func TestResolveProfile(t *testing.T) {
	root := t.TempDir()
	bob := filepath.Join(root, "profiles", "bob")
	if err := os.MkdirAll(bob, 0700); err != nil {
		t.Fatal(err)
	}

	t.Setenv(OswaldHomeEnv, bob)
	if got, err := ResolveProfile(""); err != nil || got != "bob" {
		t.Fatalf("env profile = %q %v", got, err)
	}
	if got, err := ResolveProfile("bob"); err != nil || got != "bob" {
		t.Fatalf("matching --profile rejected: %q %v", got, err)
	}
	if _, err := ResolveProfile("alice"); err == nil {
		t.Fatal("mismatched --profile accepted")
	}
	if _, err := ResolveProfile("Bad Name!"); err == nil {
		t.Fatal("invalid --profile accepted")
	}

	t.Setenv(OswaldHomeEnv, "")
	if got, err := ResolveProfile(""); err != nil || got != "default" {
		t.Fatalf("default profile = %q %v", got, err)
	}
}
