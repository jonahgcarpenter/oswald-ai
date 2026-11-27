package safety

import (
	"strings"
	"testing"
)

func TestValidateNavigateURL(t *testing.T) {
	if _, err := ValidateNavigateURL("https://example.com", false, false); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"https://example.com/?api_key=secret1234567890",
		"https://example.com/?client_secret=abcdef123456",
		"https://example.com/?password=hunter2hunter2",
		"https://example.com/?api%5Fkey=secret1234567890",
		"http://169.254.169.254/latest/meta-data",
		"https://metadata.google.internal/",
		"http://localhost:8080/",
		"http://127.0.0.1/",
		"http://example.com/",
		"ftp://example.com/file",
		"javascript:alert(1)",
		"",
	} {
		if _, err := ValidateNavigateURL(raw, false, false); err == nil {
			t.Fatalf("allowed blocked URL %q", raw)
		}
	}
	for _, raw := range []string{
		"https://example.com/token/refresh",
		"https://example.com/login?token=abc123",
		"https://example.com/callback?code=xyz&state=abc",
		"https://example.com/invite?auth=abc123",
		"https://example.com/?session_id=abc123",
		"https://example.com/?password=",
	} {
		if _, err := ValidateNavigateURL(raw, false, false); err != nil {
			t.Fatalf("blocked legitimate login-flow URL %q: %v", raw, err)
		}
	}
	if _, err := ValidateNavigateURL("http://127.0.0.1/", true, true); err != nil {
		t.Fatalf("allowPrivate+allowInsecure should permit loopback http: %v", err)
	}
	if _, err := ValidateNavigateURL("http://example.com/", false, true); err != nil {
		t.Fatalf("allowInsecure should permit public http: %v", err)
	}
	if _, err := ValidateNavigateURL("https://example.com/", false, true); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"ftp://example.com/file", "javascript:alert(1)"} {
		if _, err := ValidateNavigateURL(raw, true, true); err == nil {
			t.Fatalf("insecure flag allowed non-http scheme %q", raw)
		}
	}
	if err := CheckFinalURL("http://127.0.0.1/", false, true); err == nil || !strings.Contains(err.Error(), "private destination") {
		t.Fatalf("private final URL error = %v", err)
	}
	if err := CheckFinalURL("http://example.com/", false, false); err == nil || !strings.Contains(err.Error(), "http URL") {
		t.Fatalf("insecure final URL error = %v", err)
	}
	if err := CheckFinalURL("chrome-error://chromewebdata/", true, true); err == nil || !strings.Contains(err.Error(), "scheme chrome-error") {
		t.Fatalf("error-page final URL error = %v", err)
	}
	if err := CheckFinalURL("http://169.254.169.254/latest/meta-data", true, true); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("metadata final URL error = %v", err)
	}
	if err := CheckFinalURL("https://example.com/?password=hunter2hunter", true, true); err == nil || !strings.Contains(err.Error(), "secret parameter") {
		t.Fatalf("secret-param final URL error = %v", err)
	}
	if err := CheckFinalURL("http://example.com/", false, false); err == nil {
		t.Fatal("final URL check allowed insecure")
	}
	if err := CheckFinalURL("https://example.com/?token=abc123", false, false); err != nil {
		t.Fatalf("final URL check blocked token flow: %v", err)
	}
	if err := CheckFinalURL("https://example.com/?api_key=secret123", false, false); err == nil {
		t.Fatal("final URL check allowed secret param")
	}
	if err := CheckFinalURL("http://example.com/", false, true); err != nil {
		t.Fatalf("final URL check blocked opted-in http: %v", err)
	}
	if err := CheckFinalURL("ftp://example.com/file", true, true); err == nil {
		t.Fatal("final URL check allowed non-http scheme")
	}
	if !strings.Contains(RedactSecrets("token=abcdefghijklmnop"), "REDACTED") {
		t.Fatal("redaction did not fire")
	}
}

func TestSensitiveEvalReason(t *testing.T) {
	for _, expr := range []string{
		`document.cookie`,
		`document["cookie"]`,
		`localStorage.getItem("k")`,
		`fetch("/api")`,
		`new WebSocket("wss://example.com")`,
		`document.querySelector("input[name=email]").value`,
		`document.forms[0].value`,
	} {
		if reason := SensitiveEvalReason(expr); reason == "" {
			t.Fatalf("denylist missed %q", expr)
		}
	}
	for _, expr := range []string{
		`document.title`,
		`document.querySelectorAll("a").length`,
		`window.scrollY`,
		``,
	} {
		if reason := SensitiveEvalReason(expr); reason != "" {
			t.Fatalf("denylist hit benign %q: %s", expr, reason)
		}
	}
}
