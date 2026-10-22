package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	if _, err := os.Stat(filepath.Join(root, "vault")); !os.IsNotExist(err) {
		t.Fatal("store performed I/O on bind")
	}
	meta, err := s.AddLogin("Example", "email", "alice@example.com", "s3cret", "", "https://example.com:443/login")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Origin != "https://example.com" {
		t.Fatalf("origin = %q", meta.Origin)
	}
	if !strings.HasPrefix(meta.ID, "vault_") {
		t.Fatalf("handle = %q", meta.ID)
	}
	items, err := s.List()
	if err != nil || len(items) != 1 {
		t.Fatalf("list = %+v %v", items, err)
	}
	if items[0].Identifier != "alice@example.com" || items[0].IdentifierType != "email" {
		t.Fatalf("identifier metadata = %+v", items[0])
	}
	got, err := s.GetMeta(meta.ID)
	if err != nil || got == nil || got.Label != "Example" {
		t.Fatalf("get = %+v %v", got, err)
	}
	password, seed, err := s.ResolvePassword(meta.ID)
	if err != nil || password != "s3cret" || seed != "" {
		t.Fatalf("resolve = %q %q %v", password, seed, err)
	}
	if _, _, err := s.ResolvePassword("vault_missing"); err == nil {
		t.Fatal("resolve of unknown handle succeeded")
	}
	if removed, err := s.Remove(meta.ID); err != nil || !removed {
		t.Fatalf("remove = %v %v", removed, err)
	}
	if removed, err := s.Remove(meta.ID); err != nil || removed {
		t.Fatalf("second remove = %v %v", removed, err)
	}
	items, err = s.List()
	if err != nil || len(items) != 0 {
		t.Fatalf("list after remove = %+v %v", items, err)
	}
}

func TestStoreRejectsBadInput(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, tc := range []struct {
		label, idType, id, password, otp, origin string
	}{
		{"", "email", "a@b.c", "pw", "", "https://example.com"},
		{"x", "handle", "a@b.c", "pw", "", "https://example.com"},
		{"x", "email", "", "pw", "", "https://example.com"},
		{"x", "email", "a@b.c", "", "", "https://example.com"},
		{"x", "email", "a@b.c", "pw", "", "not a url"},
		{"x", "email", "a@b.c", "pw", "!!!", "https://example.com"},
	} {
		if _, err := s.AddLogin(tc.label, tc.idType, tc.id, tc.password, tc.otp, tc.origin); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
}

func TestStoreSurvivesReopenAndDetectsTamper(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	meta, err := s.AddLogin("Example", "email", "alice@example.com", "s3cret", "", "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	reopened := NewStore(root)
	password, _, err := reopened.ResolvePassword(meta.ID)
	if err != nil || password != "s3cret" {
		t.Fatalf("reopen resolve = %q %v", password, err)
	}
	blob, err := os.ReadFile(filepath.Join(root, "vault", "vault.json.enc"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "s3cret") {
		t.Fatal("secret stored in cleartext")
	}
	blob[len(blob)-1] ^= 0xFF
	if err := os.WriteFile(filepath.Join(root, "vault", "vault.json.enc"), blob, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reopened.ResolvePassword(meta.ID); err == nil {
		t.Fatal("tampered vault decrypted")
	}
}

func TestNormalizeOrigin(t *testing.T) {
	for url, want := range map[string]string{
		"https://Example.COM/login?a=b": "https://example.com",
		"https://example.com:443/":      "https://example.com",
		"http://example.com:80/":        "http://example.com",
		"https://example.com:8443/":     "https://example.com:8443",
	} {
		got, err := NormalizeOrigin(url)
		if err != nil || got != want {
			t.Fatalf("origin %q = %q, %v", url, got, err)
		}
	}
	for _, bad := range []string{"", "example.com", "://missing"} {
		if _, err := NormalizeOrigin(bad); err == nil {
			t.Fatalf("accepted origin %q", bad)
		}
	}
}

func TestNormalizeOTPSecret(t *testing.T) {
	seed := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	got, err := NormalizeOTPSecret("gezdgnbvgy3tqojq gezdgnbvgy3tqojq")
	if err != nil || got != seed {
		t.Fatalf("seed = %q, %v", got, err)
	}
	got, err = NormalizeOTPSecret("otpauth://totp/Example:alice?secret=" + seed + "&issuer=Example")
	if err != nil || got != seed {
		t.Fatalf("uri = %q, %v", got, err)
	}
	got, err = NormalizeOTPSecret("otpauth://totp/Example?secret=" + seed + "&digits=8&period=60&algorithm=SHA256")
	if err != nil || got != seed+"|8|60|SHA256" {
		t.Fatalf("non-default uri = %q, %v", got, err)
	}
	for _, bad := range []string{"", "!!!", "otpauth://hotp/x?secret=" + seed, "otpauth://totp/x?secret=" + seed + "&digits=5"} {
		if _, err := NormalizeOTPSecret(bad); err == nil {
			t.Fatalf("accepted otp %q", bad)
		}
	}
}

func TestTOTPVector(t *testing.T) {
	// RFC 6238 test key "12345678901234567890", T=59.
	code, err := TOTPNow("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", time.Unix(59, 0).UTC())
	if err != nil || code != "287082" {
		t.Fatalf("totp = %q, %v", code, err)
	}
	if _, err := TOTPNow("!!!", time.Now()); err == nil {
		t.Fatal("totp accepted garbage seed")
	}
}

func TestScrubRegistered(t *testing.T) {
	RegisterSecretValues("s3cret-password", "ab")
	if got := ScrubRegistered("the password is s3cret-password here"); strings.Contains(got, "s3cret") {
		t.Fatalf("scrub missed: %q", got)
	}
	if got := ScrubRegistered("short ab stays"); !strings.Contains(got, "ab stays") {
		t.Fatalf("scrub hit short value: %q", got)
	}
	if got := ScrubValues("boom s3cret-password", "s3cret-password"); strings.Contains(got, "s3cret") {
		t.Fatalf("ScrubValues missed: %q", got)
	}
}
