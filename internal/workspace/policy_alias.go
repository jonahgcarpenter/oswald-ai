package workspace

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/policy"
)

// Group aliases policy.Group so existing callers keep workspace.Group* names
// while the classifier lives in policy.
type Group = policy.Group

const (
	GroupProtected = policy.GroupProtected
	GroupSystem    = policy.GroupSystem
	GroupAgent     = policy.GroupAgent
)

// DenyError aliases policy.DenyError.
type DenyError = policy.DenyError

// Classify forwards to the policy classifier.
func Classify(abs, root string) Group {
	return policy.Classify(abs, root)
}

// AsDeny forwards to the policy helper.
func AsDeny(err error) (string, Group, bool) {
	return policy.AsDeny(err)
}

// DenyMessage forwards to the policy renderer.
func DenyMessage(rel string, group Group, write bool) string {
	return policy.DenyMessage(rel, group, write)
}

// Scan wrappers preserve workspace.Scan* names for existing tool callers
// while the implementation lives in policy.
func ScanConfinedWrite(command, workdir, root string, shell bool) (string, bool) {
	return policy.ScanConfinedWrite(command, workdir, root, shell)
}

func ScanProtectedRead(command, workdir, root string, shell bool) (string, bool) {
	return policy.ScanProtectedRead(command, workdir, root, shell)
}

func ScanEnvRetarget(text string, shell bool) (string, bool) {
	return policy.ScanEnvRetarget(text, shell)
}

func ValidateWorkdirString(raw string) error {
	return policy.ValidateWorkdirString(raw)
}
