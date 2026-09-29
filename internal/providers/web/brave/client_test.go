package brave

import "testing"

func TestNewClientRejectsMissingKey(t *testing.T) {
	if _, err := NewClient("", nil); err == nil {
		t.Fatal("accepted empty Brave key")
	}
	if client, err := NewClient("test-key", nil); err != nil || client == nil {
		t.Fatalf("client = %v, err = %v", client, err)
	}
}
