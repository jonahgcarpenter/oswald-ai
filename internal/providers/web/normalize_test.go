package web

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNormalizationBoundsTextAndEngineNames(t *testing.T) {
	t.Parallel()
	engines := make([]string, 12)
	for i := range engines {
		engines[i] = fmt.Sprintf("engine-%d-%s", i, strings.Repeat("x", 100))
	}
	result, ok := normalizeResult(Candidate{
		Title: strings.Repeat("界", 300), URL: "https://public.example/", Content: strings.Repeat("界", maxSnippetRunes+100), Engines: engines,
	})
	if !ok {
		t.Fatal("valid result rejected")
	}
	if utf8.RuneCountInString(result.Title) != maxTitleRunes || utf8.RuneCountInString(result.Snippet) != maxSnippetRunes || len(result.Engines) != maxEngineNames {
		t.Fatalf("bounds not applied: title=%d snippet=%d engines=%d", utf8.RuneCountInString(result.Title), utf8.RuneCountInString(result.Snippet), len(result.Engines))
	}
	for _, engine := range result.Engines {
		if utf8.RuneCountInString(engine) > maxEngineNameRunes {
			t.Fatalf("engine name not bounded: %q", engine)
		}
	}
}

func TestNormalizeResultRejectsMalformedQueryAndAcceptsUppercaseScheme(t *testing.T) {
	t.Parallel()
	if _, ok := normalizeResult(Candidate{Title: "bad", URL: "https://example.com/?a=1;b=2"}); ok {
		t.Fatal("result with malformed query was accepted")
	}
	result, ok := normalizeResult(Candidate{Title: "good", URL: "HTTPS://Example.com/path"})
	if !ok || result.URL != "https://example.com/path" {
		t.Fatalf("uppercase scheme result = %+v, accepted=%t", result, ok)
	}
}
