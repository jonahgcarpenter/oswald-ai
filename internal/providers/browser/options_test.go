package browser

import (
	"testing"
	"time"
)

func TestEffectiveDefaultsAndFloors(t *testing.T) {
	def := (Options{}).Effective()
	opts := def
	if opts.CommandTimeout != 30*time.Second || opts.SnapshotThreshold != 15000 || opts.InactivityTimeout != 120*time.Second {
		t.Fatalf("defaults = %+v", opts)
	}
	floored := Options{CommandTimeout: time.Second, SnapshotThreshold: 10, InactivityTimeout: time.Second}.Effective()
	if floored.CommandTimeout != 5*time.Second || floored.SnapshotThreshold != 1000 || floored.InactivityTimeout != 30*time.Second {
		t.Fatalf("floors = %+v", floored)
	}
	optsDefault := (Options{}).Effective()
	if got := optsDefault.OpenTimeout(true); got != 120*time.Second {
		t.Fatalf("first open timeout = %v", got)
	}
	if got := optsDefault.OpenTimeout(false); got != 60*time.Second {
		t.Fatalf("open timeout = %v", got)
	}
}

func TestBlockedEvalLiteralScansAllURLs(t *testing.T) {
	for _, expr := range []string{
		`fetch("http://169.254.169.254/latest/meta-data")`,
		`await (await fetch("https://metadata.google.internal/")).text()`,
		`location.href = "http://127.0.0.1:3000/admin"`,
		`var u = "http://localhost:8080/x"; fetch(u)`,
		`fetch("http://10.0.0.5/")`,
	} {
		if !blockedEvalLiteral(expr, false) {
			t.Fatalf("allowed private-URL expression %q", expr)
		}
	}
	for _, expr := range []string{
		`document.title`,
		`document.querySelectorAll("a").length`,
		`fetch("https://example.com/api")`,
	} {
		if blockedEvalLiteral(expr, false) {
			t.Fatalf("blocked benign expression %q", expr)
		}
	}
	if blockedEvalLiteral(`fetch("http://127.0.0.1/")`, true) {
		t.Fatal("allowPrivate did not bypass literal scan")
	}
}
