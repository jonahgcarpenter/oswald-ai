package process

import (
	"strings"
	"testing"
	"time"
)

func TestSpawnPollKill(t *testing.T) {
	m := NewManager(t.TempDir())
	info, err := m.Spawn("req-1", "echo hi", t.TempDir(), t.TempDir(), 30)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Wait(info.ID, 10)
	if err != nil || (got.Status != StatusCompleted && got.Status != StatusKilled) {
		t.Fatalf("wait = %+v %v", got, err)
	}
	tail, err := m.Tail(info.ID, 4096)
	if err != nil || !strings.Contains(tail, "hi") {
		t.Fatalf("tail = %q %v", tail, err)
	}
}

func TestPrefixAndKill(t *testing.T) {
	m := NewManager(t.TempDir())
	info, err := m.Spawn("req-1", "sleep 30", t.TempDir(), t.TempDir(), 60)
	if err != nil {
		t.Fatal(err)
	}
	full, err := m.Resolve(info.ID[:4])
	if err != nil || full != info.ID {
		t.Fatalf("resolve = %q %v", full, err)
	}
	killed, err := m.Kill(info.ID)
	if err != nil || (killed.Status != StatusKilled && killed.Status != StatusCompleted) {
		t.Fatalf("kill = %+v %v", killed, err)
	}
}

func TestHandoffCap(t *testing.T) {
	m := NewManager(t.TempDir())
	var ids []string
	for i := 0; i < 4; i++ {
		info, err := m.Spawn("req-h", "echo x", t.TempDir(), t.TempDir(), 30)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, info.ID)
		_, _ = m.Wait(info.ID, 5)
	}
	for i := 0; i < 3; i++ {
		if _, err := m.Handoff(ids[i], "req-h", "keep for parent review"); err != nil {
			t.Fatalf("handoff %d: %v", i, err)
		}
	}
	if _, err := m.Handoff(ids[3], "req-h", "fourth"); err == nil {
		t.Fatal("fourth handoff allowed")
	}
	_ = time.Now
}
