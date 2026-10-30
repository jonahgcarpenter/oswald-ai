package skills

import "testing"

func TestParseFrontmatterFull(t *testing.T) {
	content := "---\nname: review\ndescription: Review code\nversion: 2.0\nplatforms:\n  - discord\n  - imessage\ntags: [go, fast]\nunknown_key: dropped\n---\n\n# Body\nDo things.\n"
	front, body, ok := parseFrontmatter(content)
	if !ok {
		t.Fatal("expected ok parse")
	}
	if front.name != "review" || front.description != "Review code" || front.version != "2.0" {
		t.Fatalf("unexpected frontmatter: %+v", front)
	}
	if len(front.platforms) != 2 || front.platforms[0] != "discord" {
		t.Fatalf("unexpected platforms: %v", front.platforms)
	}
	if len(front.tags) != 2 {
		t.Fatalf("unexpected tags: %v", front.tags)
	}
	if body != "# Body\nDo things." {
		t.Fatalf("unexpected body %q", body)
	}
}

func TestParseFrontmatterQuotedAndInlineList(t *testing.T) {
	content := "---\nname: 'my skill'\ndescription: \"does stuff # not a comment\"\nplatforms: [discord, imessage]\n---\nbody\n"
	front, body, ok := parseFrontmatter(content)
	if !ok {
		t.Fatal("expected ok parse")
	}
	if front.name != "my skill" {
		t.Fatalf("unexpected name %q", front.name)
	}
	if front.description != "does stuff # not a comment" {
		t.Fatalf("unexpected description %q", front.description)
	}
	if len(front.platforms) != 2 {
		t.Fatalf("unexpected platforms %v", front.platforms)
	}
	if body != "body" {
		t.Fatalf("unexpected body %q", body)
	}
}

func TestParseFrontmatterMissingFenceIsBody(t *testing.T) {
	_, body, ok := parseFrontmatter("# Just a body\n\nSome text.\n")
	if !ok {
		t.Fatal("expected ok parse without fence")
	}
	if body == "" {
		t.Fatal("expected body content")
	}
	if got := fallbackDescription(body); got != "Some text." {
		t.Fatalf("unexpected fallback description %q", got)
	}
}

func TestParseFrontmatterUnclosedFailsClosed(t *testing.T) {
	_, _, ok := parseFrontmatter("---\nname: broken\nbody without end\n")
	if ok {
		t.Fatal("expected unclosed fence to fail")
	}
}

func TestFallbackDescriptionSkipsHeadings(t *testing.T) {
	if got := fallbackDescription("# Title\n\n\nFirst real line.\n"); got != "First real line." {
		t.Fatalf("unexpected %q", got)
	}
	if got := fallbackDescription("# Only headings\n"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}
