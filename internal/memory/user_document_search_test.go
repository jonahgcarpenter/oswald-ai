package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func publishSearchDocument(t *testing.T, s *Store, owner string, partial bool, texts ...string) UserDocument {
	t.Helper()
	d := acceptDocument(t, s, owner)
	job := claimDocument(t, s)
	chunks := make([]DocumentChunk, len(texts))
	for i, text := range texts {
		chunks[i] = DocumentChunk{Ordinal: i, Text: text, Locator: fmt.Sprintf("page %d", i+1), Method: "text"}
	}
	if err := s.CompleteUserDocumentExtraction(context.Background(), job, chunks, partial); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDocumentLexicalSearchRankingAndExcerpts(t *testing.T) {
	s := newFormationTestStore(t)
	ctx := context.Background()
	texts := []string{"Renewal is available.", "The deadline for a renewal is Sept 30.", "A prerenewal deadlineextension is unrelated."}
	// The strongest result must not disappear behind an early candidate pool or
	// prefix truncation. All chunks are valid bounded production publications.
	for i := 0; i < 200; i++ {
		texts = append(texts, "renewal information")
	}
	late := strings.Repeat("background ", 900) + "RENEWAL DEADLINE Sept30 " + strings.Repeat("appendix ", 100)
	texts = append(texts, late)
	d := publishSearchDocument(t, s, "user", false, texts...)
	results, err := s.SearchUserDocuments(ctx, "user", "What is renewal deadline?", "", 3)
	if err != nil || len(results) != 3 {
		t.Fatalf("search: %d results, %v", len(results), err)
	}
	if results[0].Chunk.Ordinal != len(texts)-1 || results[1].Chunk.Ordinal != 1 || results[2].Chunk.Ordinal != 0 {
		t.Fatalf("incorrect ranking: %+v", results)
	}
	first := results[0]
	if first.Document.ID != d.ID || first.Chunk.Locator != fmt.Sprintf("page %d", len(texts)) || first.Chunk.Method != "text" || !strings.Contains(first.Chunk.Text, "RENEWAL DEADLINE Sept30") || utf8.RuneCountInString(first.Chunk.Text) > 1500 {
		t.Fatalf("lost match or provenance: %+v", first)
	}
	read, err := s.ReadUserDocument(ctx, "user", d.ID, len(texts)-1, 1)
	if err != nil || len(read.Chunks) != 1 || read.Chunks[0].Text != late {
		t.Fatalf("canonical chunk changed: %v", err)
	}
	for i := 0; i < 2; i++ {
		again, err := s.SearchUserDocuments(ctx, "user", `"renewal deadline"`, d.ID, 1)
		if err != nil || len(again) != 1 || again[0].Chunk.Ordinal != first.Chunk.Ordinal {
			t.Fatalf("phrase ordering: %+v %v", again, err)
		}
	}
}

func TestDocumentLexicalSearchScopeAndValidation(t *testing.T) {
	s := newFormationTestStore(t)
	seedAccountUsers(t, s, "other")
	ctx := context.Background()
	live := publishSearchDocument(t, s, "user", true, "renewal deadline Sept30", "literal %_ marker", "unicode \u00c9CH\u00c9ANCE demain")
	other := publishSearchDocument(t, s, "other", false, "renewal deadline Sept30")
	expired := publishSearchDocument(t, s, "user", false, "renewal deadline Sept30")
	if _, err := s.sql.Exec(`UPDATE user_documents SET accepted_at=?,expires_at=? WHERE id=?`, time.Now().Add(-2*time.Hour).UnixMilli(), time.Now().Add(-time.Hour).UnixMilli(), expired.ID); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"queued", "extracting", "failed"} {
		d := publishSearchDocument(t, s, "user", false, "renewal deadline Sept30")
		// Deliberately retain chunks to test the serving predicate independently
		// of the extraction writer's normal state transitions.
		if _, err := s.sql.Exec(`UPDATE user_documents SET status=? WHERE id=?`, status, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	results, err := s.SearchUserDocuments(ctx, "user", "renewal deadline", "", 100)
	if err != nil || len(results) != 1 || results[0].Document.ID != live.ID || results[0].Document.Status != "partial" {
		t.Fatalf("scope: %+v %v", results, err)
	}
	for _, id := range []string{other.ID, expired.ID, "' OR 1=1 --"} {
		results, err := s.SearchUserDocuments(ctx, "user", "renewal deadline", id, 5)
		if err != nil || len(results) != 0 {
			t.Fatalf("ID scope: %+v %v", results, err)
		}
	}
	for _, query := range []string{"%_", "\u00e9ch\u00e9ance"} {
		results, err := s.SearchUserDocuments(ctx, "user", query, live.ID, 5)
		if err != nil || len(results) != 1 {
			t.Fatalf("literal/Unicode query %q: %+v %v", query, results, err)
		}
	}
	for _, query := range []string{"what is the", "' OR 1=1 --", "nonexistent", "deadlineextension"} {
		results, err := s.SearchUserDocuments(ctx, "user", query, live.ID, 5)
		if err != nil || len(results) != 0 {
			t.Fatalf("empty query match %q: %+v %v", query, results, err)
		}
	}
	for _, query := range []string{"", "  ", string([]byte{0xff}), strings.Repeat("a", 401), strings.Repeat("\u754c", 400)} {
		if _, err := s.SearchUserDocuments(ctx, "user", query, "", 1); !errors.Is(err, ErrDocumentInvalid) {
			t.Fatalf("invalid query accepted: %v", err)
		}
	}
	if _, err := s.SearchUserDocuments(ctx, "user", strings.Repeat("a", 400), "", 1); err != nil {
		t.Fatalf("400-rune query rejected: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.SearchUserDocuments(canceled, "user", "renewal", "", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestDocumentLexicalSearchTermAndSnippetBounds(t *testing.T) {
	if terms := documentSearchTerms("What is the renewal deadline for my renewal?"); strings.Join(terms, " ") != "renewal deadline" {
		t.Fatalf("terms: %v", terms)
	}
	if terms := documentSearchTerms("one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen"); len(terms) != 16 || terms[15] != "sixteen" {
		t.Fatalf("term cap: %v", terms)
	}
	text := "renewal " + strings.Repeat("\u754c ", 2500) + "renewal and deadline Sept30 " + strings.Repeat("\u754c ", 900)
	score, excerpt := documentSearchMatch(text, []string{"renewal", "deadline"})
	if score != 200 || !utf8.ValidString(excerpt) || utf8.RuneCountInString(excerpt) != 1500 || !strings.Contains(excerpt, "renewal and deadline Sept30") || !strings.Contains(text, excerpt) {
		t.Fatalf("snippet: score %d, runes %d, excerpt %q", score, utf8.RuneCountInString(excerpt), excerpt)
	}
}
