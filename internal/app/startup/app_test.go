package startup

import (
	"context"
	"errors"
	"testing"
)

func TestErrorPreservesInitializationCause(t *testing.T) {
	cause := errors.New("synthetic failure")
	err := &Error{Event: "app.test.failed", Message: "initialization failed", Cause: cause}
	if !errors.Is(err, cause) || errors.Unwrap(err) != cause || err.Error() != "initialization failed: synthetic failure" {
		t.Fatal("initialization cause lost")
	}
	if (&Error{Message: "initialization failed"}).Error() != "initialization failed" {
		t.Fatal("nil cause formatting changed")
	}
}
func TestRunPreCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}
