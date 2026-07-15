package webapp

import (
	"strings"
	"testing"
)

// A leading YAML frontmatter block renders as a key/value table (author
// key order, escaped values) instead of goldmark's thematic-break soup;
// everything that isn't a clean frontmatter mapping renders exactly as
// before.
func TestRenderMarkdownFrontmatter(t *testing.T) {
	src := `---
title: Q3 findings
tags: [churn, revenue]
owner: snow@runbear.io
meta:
  reviewed: true
---

# Body

Hello.`
	out, err := RenderMarkdown([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<table class="frontmatter">`,
		`<th scope="row">title</th><td>Q3 findings</td>`,
		`<td>churn, revenue</td>`, // flat lists comma-join
		`owner`, `snow@runbear.io`,
		`<code>reviewed: true</code>`, // nested values as compact YAML
		`<h1 id="body">Body</h1>`,     // the body still renders
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "<hr") {
		t.Errorf("frontmatter fences leaked as thematic breaks:\n%s", out)
	}
	// Key order preserved: title row precedes owner row.
	if strings.Index(out, ">title<") > strings.Index(out, ">owner<") {
		t.Errorf("frontmatter keys reordered:\n%s", out)
	}
}

func TestRenderMarkdownFrontmatterEscapes(t *testing.T) {
	out, err := RenderMarkdown([]byte("---\nnote: <script>alert(1)</script>\n---\nx"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "<script>") {
		t.Fatalf("frontmatter value not escaped:\n%s", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Fatalf("escaped value missing:\n%s", out)
	}
}

func TestRenderMarkdownFrontmatterFallthrough(t *testing.T) {
	cases := map[string]struct {
		src       string
		wantTable bool
		want      string
	}{
		"no frontmatter":   {"# Hi\n\ntext", false, "<h1"},
		"mid-doc fences":   {"para\n\n---\n\nmore", false, "<hr"},
		"unclosed fence":   {"---\ntitle: x\n\nbody", false, ""},
		"non-mapping yaml": {"---\n- just\n- a list\n---\nbody", false, ""},
		"invalid yaml":     {"---\n: : :\n---\nbody", false, ""},
		"empty frontmatter hidden": {"---\n---\nbody", false, "<p>body</p>"},
	}
	for name, c := range cases {
		out, err := RenderMarkdown([]byte(c.src))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := strings.Contains(out, `class="frontmatter"`); got != c.wantTable {
			t.Errorf("%s: table presence = %v, want %v\n%s", name, got, c.wantTable, out)
		}
		if c.want != "" && !strings.Contains(out, c.want) {
			t.Errorf("%s: missing %q in:\n%s", name, c.want, out)
		}
	}
}
