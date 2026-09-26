package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestApplyReadDeleteAndAtomicValidation(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	ctx := context.Background()
	if _, err := store.Apply(ctx, "one", "memory", []Operation{{Action: "add", Content: "first entry"}, {Action: "add", Content: "second entry"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(ctx, "one", "memory", []Operation{{Action: "replace", OldText: "first", Content: "updated"}, {Action: "remove", OldText: "missing"}}); err == nil {
		t.Fatal("expected batch rollback")
	}
	user, memory, err := store.Read(ctx, "one")
	if err != nil || user != "" || memory != "first entry\n§\nsecond entry" {
		t.Fatalf("read: %q %q %v", user, memory, err)
	}
	if _, err := store.Apply(ctx, "one", "memory", []Operation{{Action: "replace", OldText: "first", Content: "updated entry"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(ctx, "one", "memory", []Operation{{Action: "remove", OldText: "entry"}}); err == nil {
		t.Fatal("ambiguous substring accepted")
	}
	if _, err := store.Apply(ctx, "one", "user", []Operation{{Action: "add", Content: "hello"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(ctx, "two", "memory", []Operation{{Action: "add", Content: "other"}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"USER.md", "MEMORY.md", ".lock"} {
		info, err := os.Stat(filepath.Join(root, "one", name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("file mode %s: %v %v", name, info, err)
		}
	}
	info, err := os.Stat(filepath.Join(root, "one"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("directory mode: %v %v", info, err)
	}
	if err := store.Delete(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	user, memory, err = store.Read(ctx, "one")
	if err != nil || user != "" || memory != "" {
		t.Fatalf("delete: %q %q %v", user, memory, err)
	}
	_, memory, err = store.Read(ctx, "two")
	if err != nil || memory != "other" {
		t.Fatalf("other owner: %q %v", memory, err)
	}
}

func TestRejectUnsafePathsAndContent(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	ctx := context.Background()
	if _, _, err := store.Read(ctx, "absent"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "absent")); !os.IsNotExist(err) {
		t.Fatalf("read created directory: %v", err)
	}
	for _, id := range []string{"../escape", "", "a/b", "a.b", "a\n"} {
		if _, _, err := store.Read(ctx, id); err == nil {
			t.Fatalf("accepted ID %q", id)
		}
	}
	for _, content := range []string{"line\n§\nother", "§", string([]byte{0xff}), strings.Repeat("x", 2201)} {
		if _, err := store.Apply(ctx, "one", "memory", []Operation{{Action: "add", Content: content}}); err == nil {
			t.Fatalf("accepted invalid content %q", content[:min(len(content), 20)])
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "one"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(root, "one", "MEMORY.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Read(ctx, "one"); err == nil {
		t.Fatal("followed file symlink")
	}
	if _, err := store.Apply(ctx, "one", "memory", []Operation{{Action: "add", Content: "note"}}); err == nil {
		t.Fatal("replaced file symlink")
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewStore(alias).Read(ctx, "two"); err == nil {
		t.Fatal("followed root symlink")
	}
	if err := os.Symlink(filepath.Join(root, "one"), filepath.Join(root, "three")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Read(ctx, "three"); err == nil {
		t.Fatal("followed user directory symlink")
	}
	if err := os.Chmod(filepath.Join(root, "one"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Read(ctx, "one"); err == nil {
		t.Fatal("accepted public user directory")
	}
}

func TestLockHonorsCancellation(t *testing.T) {
	store := NewStore(t.TempDir())
	ctx := context.Background()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- store.locked(ctx, "one", true, func(string) error { close(entered); <-release; return nil })
	}()
	<-entered
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, _, err := store.Read(waitCtx, "one"); err != context.DeadlineExceeded {
		t.Fatalf("lock wait: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReadReflectsOperatorEditsAndRejectsOversizedExistingFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "one")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store := NewStore(root)
	ctx := context.Background()
	path := filepath.Join(dir, "USER.md")
	if err := os.WriteFile(path, []byte("operator edit"), 0600); err != nil {
		t.Fatal(err)
	}
	user, _, err := store.Read(ctx, "one")
	if err != nil || user != "operator edit" {
		t.Fatalf("operator edit: %q %v", user, err)
	}
	if err := os.WriteFile(path, []byte("later edit"), 0600); err != nil {
		t.Fatal(err)
	}
	user, _, err = store.Read(ctx, "one")
	if err != nil || user != "later edit" {
		t.Fatalf("fresh read: %q %v", user, err)
	}
	tooLong := strings.Repeat("界", 1376)
	if err := os.WriteFile(path, []byte(tooLong), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(ctx, "one", "user", []Operation{{Action: "remove", OldText: "界"}}); err == nil {
		t.Fatal("overwrote oversized existing file")
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != tooLong {
		t.Fatalf("oversized file modified: %v", err)
	}
}

func TestRuneLimitsIncludeSeparatorsAndCheckFinalBatch(t *testing.T) {
	store := NewStore(t.TempDir())
	ctx := context.Background()
	for _, tc := range []struct {
		target string
		limit  int
	}{{"user", 1375}, {"memory", 2200}} {
		t.Run(tc.target, func(t *testing.T) {
			first := strings.Repeat("界", tc.limit-4)
			result, err := store.Apply(ctx, tc.target, tc.target, []Operation{{Action: "add", Content: first}, {Action: "add", Content: "x"}})
			if err != nil || utf8.RuneCountInString(result) != tc.limit {
				t.Fatalf("exact cap: %d %v", utf8.RuneCountInString(result), err)
			}
			if _, err := store.Apply(ctx, tc.target, tc.target, []Operation{{Action: "add", Content: "y"}}); err == nil {
				t.Fatal("accepted separator beyond cap")
			}
			result, err = store.Apply(ctx, tc.target, tc.target, []Operation{{Action: "add", Content: strings.Repeat("z", tc.limit)}, {Action: "remove", OldText: strings.Repeat("z", tc.limit)}})
			if err != nil || utf8.RuneCountInString(result) != tc.limit {
				t.Fatalf("final-only cap: %d %v", utf8.RuneCountInString(result), err)
			}
			if _, err := store.Apply(ctx, tc.target, tc.target, []Operation{{Action: "replace", OldText: "x", Content: "xy"}}); err == nil {
				t.Fatal("accepted oversized final state")
			}
		})
	}
}

func TestSeparateStoresSerializeWrites(t *testing.T) {
	root := t.TempDir()
	stores := []*Store{NewStore(root), NewStore(root)}
	const count = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for index, store := range stores {
		wg.Add(1)
		go func(index int, store *Store) {
			defer wg.Done()
			<-start
			for i := 0; i < count; i++ {
				if _, err := store.Apply(context.Background(), "one", "memory", []Operation{{Action: "add", Content: strings.Repeat(string(rune('a'+index)), i+1)}}); err != nil {
					errs <- err
					return
				}
			}
		}(index, store)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	_, memory, err := stores[0].Read(context.Background(), "one")
	if err != nil || len(strings.Split(memory, "\n§\n")) != 2*count {
		t.Fatalf("lost concurrent edits: %d %v", len(strings.Split(memory, "\n§\n")), err)
	}
}

func TestDeleteMissingRetryAndCancellation(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	ctx := context.Background()
	if err := store.Delete(ctx, "absent"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "absent")); !os.IsNotExist(err) {
		t.Fatalf("delete created directory: %v", err)
	}
	if _, err := store.Apply(ctx, "one", "user", []Operation{{Action: "add", Content: "private"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(ctx, "one", "memory", []Operation{{Action: "add", Content: "other"}}); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(root, "one", "MEMORY.md")
	if err := os.Chmod(blocked, 0644); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "one"); err == nil {
		t.Fatal("deleted despite unsafe file")
	}
	if _, err := os.Stat(filepath.Join(root, "one", "USER.md")); err != nil {
		t.Fatalf("deleted first file on validation failure: %v", err)
	}
	if err := os.Chmod(blocked, 0600); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.Delete(canceled, "one"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := os.Stat(blocked); err != nil {
		t.Fatalf("canceled deletion removed file: %v", err)
	}
	if err := store.Delete(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "one"); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	user, memory, err := store.Read(ctx, "one")
	if err != nil || user != "" || memory != "" {
		t.Fatalf("delete did not clear files: %q %q %v", user, memory, err)
	}
}

func TestRejectUnsafeLockAndPublicFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "one")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, ".lock")
	if err := os.Symlink(filepath.Join(root, "outside"), lock); err != nil {
		t.Fatal(err)
	}
	store := NewStore(root)
	if _, err := store.Apply(context.Background(), "one", "user", []Operation{{Action: "add", Content: "secret"}}); err == nil {
		t.Fatal("accepted symlinked lock")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Read(context.Background(), "one"); err == nil {
		t.Fatal("accepted public lock")
	}
	if err := os.Chmod(lock, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "USER.md")
	if err := os.WriteFile(path, []byte("private"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Read(context.Background(), "one"); err == nil {
		t.Fatal("accepted public user file")
	}
	if _, err := store.Apply(context.Background(), "one", "user", []Operation{{Action: "add", Content: "new"}}); err == nil {
		t.Fatal("overwrote public user file")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "private" {
		t.Fatalf("unsafe file modified: %q %v", data, err)
	}
}

func TestDeleteCanceledWhileWaitingForSeparateStore(t *testing.T) {
	root := t.TempDir()
	writer, remover := NewStore(root), NewStore(root)
	ctx := context.Background()
	if _, err := writer.Apply(ctx, "one", "user", []Operation{{Action: "add", Content: "keep"}}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- writer.locked(ctx, "one", false, func(string) error { close(entered); <-release; return nil })
	}()
	<-entered
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if err := remover.Delete(waitCtx, "one"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("delete wait: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	user, _, err := remover.Read(ctx, "one")
	if err != nil || user != "keep" {
		t.Fatalf("canceled delete removed file: %q %v", user, err)
	}
	if err := remover.Delete(ctx, "one"); err != nil {
		t.Fatalf("retry delete: %v", err)
	}
}
