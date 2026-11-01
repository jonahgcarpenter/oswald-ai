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
	if body != "# Body\nDo things." {
		t.Fatalf("unexpected body %q", body)
	}
}

func TestParseFrontmatterIgnoresHermesHeaders(t *testing.T) {
	content := "---\nname: github\ndescription: \"GitHub via gh CLI: PRs, issues, reviews.\"\nversion: 2.0.0\nauthor: Ben Barclay\nlicense: MIT\nplatforms: [linux, macos, windows]\nmetadata:\n  hermes:\n    tags: [github, gh]\n    category: software-development\n---\nbody\n"
	front, _, ok := parseFrontmatter(content)
	if !ok {
		t.Fatal("expected ok parse")
	}
	if front.name != "github" || front.description != "GitHub via gh CLI: PRs, issues, reviews." || front.version != "2.0.0" {
		t.Fatalf("unexpected frontmatter: %+v", front)
	}
}

func TestParseFrontmatterIgnoresNestedKeys(t *testing.T) {
	content := "---\nname: top\ndescription: Top level\nmetadata:\n  description: nested must not win\n  tags:\n    - leaked\n---\nbody\n"
	front, _, ok := parseFrontmatter(content)
	if !ok {
		t.Fatal("expected ok parse")
	}
	if front.description != "Top level" {
		t.Fatalf("nested key leaked: %+v", front)
	}
}

func TestParseFrontmatterQuotedAndInlineList(t *testing.T) {
	content := "---\nname: 'my skill'\ndescription: \"does stuff # not a comment\"\nversion: 1.0\n---\nbody\n"
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
