package cron

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestJobScriptScopeOverrideRejected(t *testing.T) {
	root := t.TempDir()
	writeScript(t, root, "scope.sh", "#!/bin/sh\nOSWALD_HOME=/tmp/x oswald env show\n")
	if err := scanScriptScope(filepath.Join(root, "scripts", "scope.sh")); err == nil || !strings.Contains(err.Error(), "OSWALD_*") {
		t.Fatalf("scope scan = %v", err)
	}
	if _, err := runScriptFile(context.Background(), testRunner(root), "scope.sh", 5); err == nil || !strings.Contains(err.Error(), "OSWALD_*") {
		t.Fatalf("run allowed scope override: %v", err)
	}
	writeScript(t, root, "safe.sh", "#!/bin/sh\nprintf '%s' \"$OSWALD_HOME\"\n")
	if err := scanScriptScope(filepath.Join(root, "scripts", "safe.sh")); err != nil {
		t.Fatalf("safe script blocked: %v", err)
	}
}
