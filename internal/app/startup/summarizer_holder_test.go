package startup

import (
	"context"
	"sync"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
)

type fakeSummarizerTarget struct {
	mu       sync.Mutex
	name     string
	calls    int
	gets     int
	sets     int
	markdown string
}

func (f *fakeSummarizerTarget) Summarize(_ context.Context, _ []memory.SessionTurn) (string, *llm.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.name, nil, nil
}

func (f *fakeSummarizerTarget) Get(_, _ string, _ int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	return f.markdown
}

func (f *fakeSummarizerTarget) Set(_, _ string, _ int, markdown string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	f.markdown = markdown
}

func (f *fakeSummarizerTarget) snapshot() (calls, gets, sets int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.gets, f.sets
}

func TestSummarizerHolderDelegatesAndRotates(t *testing.T) {
	first := &fakeSummarizerTarget{name: "first"}
	second := &fakeSummarizerTarget{name: "second"}
	holder := newSummarizerHolder(first)
	ctx := context.Background()

	if got, _, err := holder.Summarize(ctx, nil); err != nil || got != "first" {
		t.Fatalf("summarize = %q err=%v", got, err)
	}
	holder.Set("alice", "discord:dm:1", 1, "markdown")
	if got := holder.Get("alice", "discord:dm:1", 1); got != "markdown" {
		t.Fatalf("get = %q", got)
	}
	if calls, gets, sets := first.snapshot(); calls != 1 || gets != 1 || sets != 1 {
		t.Fatalf("first target calls=%d gets=%d sets=%d", calls, gets, sets)
	}

	holder.Rotate(second)
	if got, _, err := holder.Summarize(ctx, nil); err != nil || got != "second" {
		t.Fatalf("rotated summarize = %q err=%v", got, err)
	}
	if got := holder.Get("alice", "discord:dm:1", 1); got != "" {
		t.Fatalf("rotated get reused the old cache: %q", got)
	}
	if calls, _, _ := second.snapshot(); calls != 1 {
		t.Fatalf("second target calls = %d", calls)
	}
}

func TestSummarizerHolderIgnoresNilRotate(t *testing.T) {
	first := &fakeSummarizerTarget{name: "first"}
	holder := newSummarizerHolder(first)
	holder.Rotate(nil)
	if got, _, err := holder.Summarize(context.Background(), nil); err != nil || got != "first" {
		t.Fatalf("nil rotate cleared the summarizer: %q %v", got, err)
	}

	var nilHolder *summarizerHolder
	nilHolder.Rotate(first)
	if _, _, err := nilHolder.Summarize(context.Background(), nil); err == nil {
		t.Fatal("nil holder summarized")
	}
	if got := nilHolder.Get("alice", "discord:dm:1", 1); got != "" {
		t.Fatalf("nil holder returned %q", got)
	}
	nilHolder.Set("alice", "discord:dm:1", 1, "markdown")
}

func TestSummarizerHolderConcurrentRotate(t *testing.T) {
	first := &fakeSummarizerTarget{name: "first"}
	second := &fakeSummarizerTarget{name: "second"}
	holder := newSummarizerHolder(first)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for index := 0; ; index++ {
			select {
			case <-stop:
				return
			default:
				if index%2 == 0 {
					holder.Rotate(second)
				} else {
					holder.Rotate(first)
				}
			}
		}
	}()
	ctx := context.Background()
	for range 200 {
		got, _, err := holder.Summarize(ctx, nil)
		if err != nil || (got != "first" && got != "second") {
			t.Errorf("concurrent summarize = %q err=%v", got, err)
			break
		}
		holder.Set("alice", "discord:dm:1", 1, got)
		_ = holder.Get("alice", "discord:dm:1", 1)
	}
	close(stop)
	wg.Wait()
}
