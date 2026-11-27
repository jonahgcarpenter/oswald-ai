package providers

import (
	"strings"
	"testing"
)

func TestEditAPIAndKeyRoundTrip(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, stderr := run(t, root, "edit", "fake", "--api", "http://127.0.0.1:9998/v1"); code != 0 {
		t.Fatalf("edit api = %d %s", code, stderr)
	}
	if code, out, _ := run(t, root, "show", "fake"); code != 0 || !strings.Contains(out, "9998") {
		t.Fatalf("show after edit = %d %q", code, out)
	}
	if code, _, stderr := run(t, root, "edit", "fake", "--key", "rotated-key"); code != 0 {
		t.Fatalf("edit key = %d %s", code, stderr)
	}
	if code, out, _ := run(t, root, "show", "fake"); code != 0 || !strings.Contains(out, "[redacted]") {
		t.Fatalf("show after key edit = %d %q", code, out)
	}
}

func TestEditAuthKindNeedsClearKey(t *testing.T) {
	root := writeConfig(t, testConfig)
	// A static key alongside a subscription auth kind fails closed.
	if code, _, _ := run(t, root, "edit", "fake", "--auth", "chatgpt-oauth"); code == 0 {
		t.Fatal("subscription auth with lingering key accepted")
	}
	if code, _, stderr := run(t, root, "edit", "fake", "--auth", "chatgpt-oauth", "--clear-key", "--clear-api"); code != 0 {
		t.Fatalf("edit to subscription = %d %s", code, stderr)
	}
	if code, _, _ := run(t, root, "edit", "fake", "--auth", "bogus"); code == 0 {
		t.Fatal("unknown auth kind accepted")
	}
	if code, _, _ := run(t, root, "edit", "fake"); code == 0 {
		t.Fatal("edit without leaves accepted")
	}
	if code, _, _ := run(t, root, "edit", "ghost", "--api", "http://127.0.0.1:1/v1"); code == 0 {
		t.Fatal("edit of unknown provider accepted")
	}
}
