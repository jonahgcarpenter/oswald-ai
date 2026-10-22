package browser

import (
	"encoding/json"
	"testing"
)

func dialogParams(t *testing.T, payload map[string]string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDialogEventTracking(t *testing.T) {
	p := &cdpPage{}
	if _, ok := p.PendingDialog(); ok {
		t.Fatal("dialog pending before any event")
	}
	p.handleEvent("Page.javascriptDialogOpening", dialogParams(t, map[string]string{
		"url": "https://example.com", "message": "Are you sure?", "type": "confirm",
	}))
	pending, ok := p.PendingDialog()
	if !ok {
		t.Fatal("dialog not tracked after opening event")
	}
	if pending.ID != "1" || pending.Type != "confirm" || pending.Message != "Are you sure?" {
		t.Fatalf("pending dialog = %+v", pending)
	}
	p.handleEvent("Page.javascriptDialogOpening", dialogParams(t, map[string]string{
		"url": "https://example.com", "message": "Second?", "type": "alert",
	}))
	if pending, ok := p.PendingDialog(); !ok || pending.ID != "2" {
		t.Fatalf("second dialog = %+v %v", pending, ok)
	}
	p.handleEvent("Page.javascriptDialogClosed", dialogParams(t, map[string]string{}))
	if _, ok := p.PendingDialog(); ok {
		t.Fatal("dialog still pending after close event")
	}
	if err := p.RespondDialog(t.Context(), true, ""); err == nil {
		t.Fatal("respond without open dialog succeeded")
	}
}
