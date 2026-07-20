package clientauth

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/websocketauth"
)

type fakeService struct {
	clients       []websocketauth.Client
	revokedClient string
	approvedCode  string
	bootstrapCode string
}

func (f *fakeService) ApproveForUser(_ context.Context, _, _, code string) (string, error) {
	f.approvedCode = code
	return "ws_subject", nil
}
func (f *fakeService) ApproveNewUser(context.Context, string, string, bool) (string, error) {
	return "new_user", nil
}
func (f *fakeService) ListClients(context.Context, string) ([]websocketauth.Client, error) {
	return f.clients, nil
}
func (f *fakeService) RevokeClient(_ context.Context, _, clientID string) error {
	f.revokedClient = clientID
	return nil
}
func (f *fakeService) BootstrapAdmin(_ context.Context, code, _, _, _ string) (string, error) {
	f.bootstrapCode = code
	return "permanent_admin", nil
}

type fakeAuthorizer bool

func (f fakeAuthorizer) IsAdmin(string) (bool, error) { return bool(f), nil }

func TestClientCommandsApproveAndRevoke(t *testing.T) {
	service := &fakeService{clients: []websocketauth.Client{{ClientID: "other_client", ClientName: "Laptop"}}}
	handler := New(service, fakeAuthorizer(true))
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "websocket", ExternalID: "subject", Assurance: identity.AssuranceWebSocketSignedToken}
	if _, err := handler.Execute(context.Background(), commands.Request{Name: "client", Args: []string{"approve", "ABCD-EFGH"}, Principal: principal, IsDirect: true}); err != nil || service.approvedCode != "ABCD-EFGH" {
		t.Fatalf("approve result: code=%q err=%v", service.approvedCode, err)
	}
	result, err := handler.Execute(context.Background(), commands.Request{Name: "client", Args: []string{"revoke", "other_client"}, Principal: principal, ClientID: "current_client", IsDirect: true})
	if err != nil || service.revokedClient != "other_client" || result.Invalidation == nil || result.Invalidation.ExternalIdentities[0] != "websocket-client:other_client" {
		t.Fatalf("revoke result: result=%+v client=%q err=%v", result, service.revokedClient, err)
	}
}

func TestBootstrapCommandRequiresTemporaryClient(t *testing.T) {
	service := &fakeService{}
	handler := NewBootstrap(service)
	principal := identity.Principal{CanonicalUserID: "bootstrap_user", Gateway: "websocket", ExternalID: "subject", Assurance: identity.AssuranceWebSocketSignedToken}
	result, err := handler.Execute(context.Background(), commands.Request{Name: "bootstrap", Args: []string{"admin", "ABCD-EFGH", "Permanent", "Admin"}, Principal: principal, ClientID: "bootstrap_client", IsDirect: true})
	if err != nil || service.bootstrapCode != "ABCD-EFGH" || result.Text == "" {
		t.Fatalf("bootstrap result=%+v code=%q err=%v", result, service.bootstrapCode, err)
	}
}
