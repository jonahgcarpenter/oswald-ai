package websocketauth

import (
	"context"
	"database/sql"
	"testing"
)

func TestMergeUsersAndExternalIdentities(t *testing.T) {
	test := newTestStore(t)
	ctx := context.Background()
	addUser(t, test.db.SQL(), "winner")
	addUser(t, test.db.SQL(), "loser")
	device, _ := test.store.RequestDevice(ctx, "Merged Client")
	identifier, err := test.store.ApproveForUser(ctx, "loser", "Loser", device.UserCode)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := test.store.PollDevice(ctx, device.DeviceCode)
	if err != nil {
		t.Fatal(err)
	}

	tx, err := test.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	identities, err := ExternalIdentitiesTx(ctx, tx, "loser")
	if err != nil || len(identities) != 1 || identities[0] != "websocket:"+identifier {
		_ = tx.Rollback()
		t.Fatalf("identities=%v err=%v", identities, err)
	}
	if err := MergeUsersTx(ctx, tx, "winner", "loser"); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_users WHERE canonical_user_id = 'loser'`); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var clientOwner, authorizationOwner string
	if err := test.db.SQL().QueryRow(`SELECT canonical_user_id FROM websocket_clients WHERE client_id = ?`, pair.ClientID).Scan(&clientOwner); err != nil {
		t.Fatal(err)
	}
	if err := test.db.SQL().QueryRow(`SELECT target_user_id FROM websocket_device_authorizations WHERE device_code_hash = ?`, hashOpaque(device.DeviceCode)).Scan(&authorizationOwner); err != nil {
		t.Fatal(err)
	}
	if clientOwner != "winner" || authorizationOwner != "winner" {
		t.Fatalf("owners client=%q authorization=%q", clientOwner, authorizationOwner)
	}

	if _, err := test.db.SQL().Exec(`DELETE FROM account_users WHERE canonical_user_id = 'winner'`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"websocket_clients", "websocket_device_authorizations"} {
		var count int
		if err := test.db.SQL().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s rows after user deletion = %d", table, count)
		}
	}
}
