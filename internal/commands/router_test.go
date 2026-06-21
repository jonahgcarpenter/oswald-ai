package commands

import (
	"errors"
	"testing"
)

func TestRouterDispatchesFirstMatchingHandler(t *testing.T) {
	first := &fakeHandler{can: true, handled: false}
	second := &fakeHandler{can: true, response: "ok", handled: true}
	router := NewRouter(nil, first, second)

	if !router.IsCommand("/test") {
		t.Fatal("expected command")
	}
	response, handled, err := router.Handle("user", "/test")
	if err != nil || !handled || response != "ok" {
		t.Fatalf("unexpected handle result response=%q handled=%v err=%v", response, handled, err)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("unexpected handler calls first=%d second=%d", first.calls, second.calls)
	}
}

func TestRouterStopsOnHandlerError(t *testing.T) {
	wantErr := errors.New("boom")
	first := &fakeHandler{can: true, err: wantErr}
	second := &fakeHandler{can: true, response: "ok", handled: true}
	router := NewRouter(first, second)

	_, handled, err := router.Handle("user", "/test")
	if err != wantErr || handled {
		t.Fatalf("expected first handler error, handled=%v err=%v", handled, err)
	}
	if second.calls != 0 {
		t.Fatalf("expected second handler skipped, calls=%d", second.calls)
	}
}

func TestNilRouterIsSafe(t *testing.T) {
	var router *Router
	if router.IsCommand("/test") {
		t.Fatal("nil router should not match commands")
	}
	response, handled, err := router.Handle("user", "/test")
	if response != "" || handled || err != nil {
		t.Fatalf("unexpected nil handle result response=%q handled=%v err=%v", response, handled, err)
	}
}

type fakeHandler struct {
	can      bool
	response string
	handled  bool
	err      error
	calls    int
}

func (h *fakeHandler) CanHandle(string) bool { return h.can }

func (h *fakeHandler) Handle(string, string) (string, bool, error) {
	h.calls++
	return h.response, h.handled, h.err
}
