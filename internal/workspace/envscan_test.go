package workspace

import "testing"

func TestScanEnvRetargetShell(t *testing.T) {
	for _, cmd := range []string{
		`OSWALD_HOME=/home/oswald/.oswald/profiles/jonah oswald env show`,
		`export OSWALD_HOME=/home/oswald/.oswald`,
		`env OSWALD_WORKSPACE=/tmp/x oswald mcp list`,
		`env -u OSWALD_HOME oswald env show`,
		`unset OSWALD_HOME`,
		`env -i oswald env show`,
		`sh -c 'OSWALD_HOME=/tmp/x oswald env show'`,
		`node -e "process.env.OSWALD_HOME='/tmp/x'"`,
		`node -e "process.env['OSWALD_HOME'] = '/tmp/x'"`,
	} {
		if _, blocked := ScanEnvRetarget(cmd, true); !blocked {
			t.Fatalf("scope override missed: %q", cmd)
		}
	}
	for _, cmd := range []string{
		`printf '%s' "$OSWALD_HOME"`,
		`echo 'OSWALD_HOME=/tmp/x'`,
		`oswald env show`,
		`go test ./...`,
		`FOO=bar ./script`,
		`env -i /usr/bin/env`,
		`grep -r HA_MCP .`,
	} {
		if reason, blocked := ScanEnvRetarget(cmd, true); blocked {
			t.Fatalf("safe command blocked %q: %s", cmd, reason)
		}
	}
}

func TestScanEnvRetargetCode(t *testing.T) {
	for _, code := range []string{
		`import os; os.environ["OSWALD_HOME"] = "/tmp/x"`,
		`os.environ['OSWALD_WORKSPACE'] = root`,
		`os.putenv("OSWALD_HOME", root)`,
		`del os.environ["OSWALD_HOME"]`,
		`os.environ.pop("OSWALD_HOME", None)`,
		`os.environ.update({"OSWALD_HOME": root})`,
	} {
		if _, blocked := ScanEnvRetarget(code, false); !blocked {
			t.Fatalf("scope mutation missed: %q", code)
		}
	}
	for _, code := range []string{
		`import os; print(os.environ.get("OSWALD_HOME", ""))`,
		`print(os.environ["PATH"])`,
		`print("OSWALD_HOME is pinned")`,
	} {
		if reason, blocked := ScanEnvRetarget(code, false); blocked {
			t.Fatalf("safe code blocked %q: %s", code, reason)
		}
	}
}
