package websocketauth

import (
	"context"
	"errors"
	"testing"
)

func TestBootstrapRecoveryPermanentAdminAndCompletion(t *testing.T) {
	test := newTestStore(t)
	ctx := context.Background()
	first, err := test.store.EnsureBootstrap(ctx)
	if err != nil || first == nil {
		t.Fatalf("first bootstrap=%+v err=%v", first, err)
	}
	if first.DefaultUserID == "" || first.ClientID == "" || first.WebSocketIdentifier == "" || first.AccessToken == "" {
		t.Fatalf("incomplete bootstrap: %+v", first)
	}
	second, err := test.store.EnsureBootstrap(ctx)
	if err != nil || second == nil {
		t.Fatalf("recovered bootstrap=%+v err=%v", second, err)
	}
	if first.ClientID != second.ClientID || first.DefaultUserID != second.DefaultUserID || first.AccessToken == second.AccessToken {
		t.Fatal("bootstrap recovery did not replace the same client credential")
	}
	if _, err := test.store.VerifyAccess(ctx, first.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old bootstrap JWT error = %v", err)
	}
	bootstrapIdentity, err := test.store.VerifyAccess(ctx, second.AccessToken)
	if err != nil || !bootstrapIdentity.IsBootstrap {
		t.Fatalf("bootstrap identity=%+v err=%v", bootstrapIdentity, err)
	}

	device, _ := test.store.RequestDevice(ctx, "Permanent Admin Device")
	permanentUserID, err := test.store.BootstrapAdmin(ctx, device.UserCode, "Permanent Admin", second.DefaultUserID, second.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	if permanentUserID == second.DefaultUserID {
		t.Fatal("permanent administrator reused bootstrap user")
	}
	if _, err := test.store.BootstrapAdmin(ctx, device.UserCode, "Other", second.DefaultUserID, second.ClientID); !errors.Is(err, ErrBootstrapUnavailable) {
		t.Fatalf("duplicate bootstrap admin error = %v", err)
	}
	if completion, err := test.store.CompleteBootstrapOnAdminConnection(ctx, "wrong-user"); err != nil || completion != nil {
		t.Fatalf("wrong-user completion=%+v err=%v", completion, err)
	}
	completion, err := test.store.CompleteBootstrapOnAdminConnection(ctx, permanentUserID)
	if err != nil || completion == nil || completion.ClientID != second.ClientID || completion.DefaultUserID != second.DefaultUserID {
		t.Fatalf("completion=%+v err=%v", completion, err)
	}
	if _, err := test.store.VerifyAccess(ctx, second.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("completed bootstrap JWT error = %v", err)
	}
	var state string
	var admin int
	if err := test.db.SQL().QueryRow(`SELECT state FROM websocket_bootstrap_state WHERE singleton_id = 1`).Scan(&state); err != nil || state != "completed" {
		t.Fatalf("bootstrap state=%q err=%v", state, err)
	}
	if err := test.db.SQL().QueryRow(`SELECT is_admin FROM account_users WHERE canonical_user_id = ?`, permanentUserID).Scan(&admin); err != nil || admin != 1 {
		t.Fatalf("permanent admin=%d err=%v", admin, err)
	}
}

func TestBootstrapDoesNothingForExistingDeployment(t *testing.T) {
	test := newTestStore(t)
	addUser(t, test.db.SQL(), "existing")
	credentials, err := test.store.EnsureBootstrap(context.Background())
	if err != nil || credentials != nil {
		t.Fatalf("bootstrap=%+v err=%v", credentials, err)
	}
	var count int
	if err := test.db.SQL().QueryRow(`SELECT COUNT(*) FROM websocket_bootstrap_state`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("bootstrap state count=%d err=%v", count, err)
	}
}
