package routing

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDocumentOnlyRoutingKeepsAuthoredPromptEmpty(t *testing.T) {
	d := Decide(Input{HasDocuments: true})
	if d.Action != ActionLLM || d.Prompt != "" {
		t.Fatalf("decision=%+v", d)
	}
	if Decide(Input{HasDocuments: true, IsGroup: true}).Action != ActionIgnore {
		t.Fatal("uninvoked group upload admitted")
	}
}

func TestDocumentLoaderIsLazyBoundedAndCancelable(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, "document") }))
	defer server.Close()
	origin, _ := url.Parse(server.URL)
	allow := func(u *url.URL) bool { return u.Scheme == origin.Scheme && u.Host == origin.Host }
	docs := []DocumentDownload{{Filename: "notes.txt", URL: server.URL}}
	loader := NewDocumentLoader(server.Client(), docs, allow)
	if loader.FileCount != 1 || loader.SourceBytes != 20<<20 {
		t.Fatal("unknown size did not reserve full file ceiling")
	}
	if calls.Load() != 0 {
		t.Fatal("download was eager")
	}
	got, err := loader.Load(context.Background())
	if err != nil || len(got) != 1 || string(got[0].Data) != "document" || got[0].MediaType == "" {
		t.Fatalf("uploads=%v err=%v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = loader.Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	docs[0].Size = (20 << 20) + 1
	before := calls.Load()
	if _, err = NewDocumentLoader(server.Client(), docs, allow).Load(context.Background()); err == nil {
		t.Fatal("oversized declaration accepted")
	}
	if calls.Load() != before {
		t.Fatal("oversized declaration downloaded")
	}
	docs[0].Size = 4
	if _, err = NewDocumentLoader(server.Client(), docs, allow).Load(context.Background()); err == nil {
		t.Fatal("actual data exceeded declared reservation")
	}
	before = calls.Load()
	declared := []DocumentDownload{{Filename: "a.txt", URL: server.URL, Size: 20 << 20}, {Filename: "b.txt", URL: server.URL, Size: 20 << 20}, {Filename: "c.txt", URL: server.URL, Size: 1}}
	if _, err = NewDocumentLoader(server.Client(), declared, allow).Load(context.Background()); err == nil || calls.Load() != before {
		t.Fatal("oversized aggregate declaration issued GET")
	}
	if _, err = NewDocumentLoader(server.Client(), append(docs, docs[0], docs[0], docs[0], docs[0]), allow).Load(context.Background()); err == nil {
		t.Fatal("five files accepted")
	}
}

func TestDocumentLoaderRejectsCrossOriginBeforeCredentialForwarding(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/?password=secret", http.StatusFound)
	}))
	defer server.Close()
	origin, _ := url.Parse(server.URL)
	loader := NewDocumentLoader(server.Client(), []DocumentDownload{{Filename: "doc.pdf", URL: server.URL + "/?password=secret"}}, func(u *url.URL) bool { return u.Host == origin.Host && u.Scheme == origin.Scheme })
	if _, err := loader.Load(context.Background()); err == nil {
		t.Fatal("cross-origin redirect accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("credential-bearing request reached other origin")
	}
}

func TestDocumentLoaderBoundsActualStreamAndAggregate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := 20 << 20
		if r.URL.Path == "/large" {
			n++
		}
		_, _ = io.CopyN(w, strings.NewReader(strings.Repeat("x", n)), int64(n))
	}))
	defer server.Close()
	allow := func(u *url.URL) bool { return true }
	docs := []DocumentDownload{{Filename: "doc.txt", URL: server.URL + "/large"}}
	if _, err := NewDocumentLoader(server.Client(), docs, allow).Load(context.Background()); err == nil {
		t.Fatal("oversized stream accepted")
	}
	docs[0].URL = server.URL
	docs = append(docs, docs[0], docs[0])
	if _, err := NewDocumentLoader(server.Client(), docs, allow).Load(context.Background()); err == nil {
		t.Fatal("aggregate over 40 MiB accepted")
	}
}
