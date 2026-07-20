package soul

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
)

type testSoulAuthorizer struct {
	admin bool
	err   error
}

func (a testSoulAuthorizer) IsAdmin(string) (bool, error) { return a.admin, a.err }

func TestSoulPatchRequiresAuthenticatedAdministrator(t *testing.T) {
	store := NewStore(t.TempDir()+"/soul.md", config.NewLogger(config.LevelError))
	args := map[string]interface{}{"operation": "add", "content": "Oswald uses Go.", "position": "end"}
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "websocket", ExternalID: "user", Assurance: identity.AssuranceWebSocketSignedToken}
	ctx := requestctx.WithPrincipal(context.Background(), principal)
	if _, err := NewPatchHandler(store, testSoulAuthorizer{}, config.NewLogger(config.LevelError))(ctx, args); err == nil {
		t.Fatal("non-admin patched soul")
	}
	if content, _ := store.Read(); content != "" {
		t.Fatalf("denied patch mutated soul: %q", content)
	}
	if _, err := NewPatchHandler(store, testSoulAuthorizer{admin: true}, config.NewLogger(config.LevelError))(ctx, args); err != nil {
		t.Fatal(err)
	}
	if content, _ := store.Read(); content != "Oswald uses Go." {
		t.Fatalf("admin patch content=%q", content)
	}
	selfAsserted := principal
	selfAsserted.Assurance = identity.AssuranceSelfAsserted
	if _, err := NewPatchHandler(store, testSoulAuthorizer{admin: true}, config.NewLogger(config.LevelError))(requestctx.WithPrincipal(context.Background(), selfAsserted), args); err == nil {
		t.Fatal("self-asserted principal patched soul")
	}
}
