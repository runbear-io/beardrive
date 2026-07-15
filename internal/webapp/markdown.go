package webapp

import (
	"bytes"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"gopkg.in/yaml.v3"
)

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
)

// wikiRe matches Obsidian-style [[target]] and [[target|label]] links.
var wikiRe = regexp.MustCompile(`\[\[([^\]|]+)(?:\|([^\]]+))?\]\]`)

// expandWikilinks rewrites [[target]] to a markdown link with a wiki: URL;
// the frontend resolves the target against the file tree by basename.
func expandWikilinks(src []byte) []byte {
	return wikiRe.ReplaceAllFunc(src, func(m []byte) []byte {
		g := wikiRe.FindSubmatch(m)
		target, label := g[1], g[2]
		if len(label) == 0 {
			label = target
		}
		return []byte("[" + string(label) + "](wiki:" + url.PathEscape(string(target)) + ")")
	})
}

// RenderMarkdown converts markdown to HTML (GFM + wikilinks). Raw HTML in
// the source is escaped by goldmark's safe default. A leading YAML
// frontmatter block renders as a small key/value table instead of the
// broken thematic-break soup goldmark would make of it.
func RenderMarkdown(src []byte) (string, error) {
	table, body := frontmatterTable(src)
	var buf bytes.Buffer
	if err := md.Convert(expandWikilinks(body), &buf); err != nil {
		return "", err
	}
	return table + buf.String(), nil
}

// fmCloseRe matches a frontmatter closing fence on its own line.
var fmCloseRe = regexp.MustCompile(`(?m)^(---|\.\.\.)\s*$`)

// frontmatterTable splits a leading YAML frontmatter block off src and
// renders it as an HTML table (keys in author order, values escaped).
// Anything that isn't a well-formed YAML mapping falls through untouched —
// a stray --- line must keep rendering exactly as it always did.
func frontmatterTable(src []byte) (string, []byte) {
	rest, ok := bytes.CutPrefix(src, []byte("---\n"))
	if !ok {
		if rest, ok = bytes.CutPrefix(src, []byte("---\r\n")); !ok {
			return "", src
		}
	}
	loc := fmCloseRe.FindIndex(rest)
	if loc == nil {
		return "", src
	}
	fm, body := rest[:loc[0]], rest[loc[1]:]
	var doc yaml.Node
	if yaml.Unmarshal(fm, &doc) != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "", src
	}
	m := doc.Content[0]
	if len(m.Content) == 0 {
		return "", body // empty frontmatter: hide it, nothing to tabulate
	}
	var b strings.Builder
	b.WriteString(`<table class="frontmatter"><tbody>`)
	for i := 0; i+1 < len(m.Content); i += 2 {
		key, val := m.Content[i], m.Content[i+1]
		fmt.Fprintf(&b, `<tr><th scope="row">%s</th><td>%s</td></tr>`,
			html.EscapeString(key.Value), yamlValueHTML(val))
	}
	b.WriteString(`</tbody></table>`)
	return b.String(), body
}

// yamlValueHTML renders one frontmatter value: scalars as text, flat lists
// comma-joined, anything nested as compact YAML in a <code> block. Always
// escaped — frontmatter is user input, never markup.
func yamlValueHTML(n *yaml.Node) string {
	switch n.Kind {
	case yaml.ScalarNode:
		return html.EscapeString(n.Value)
	case yaml.SequenceNode:
		flat := true
		parts := make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				flat = false
				break
			}
			parts = append(parts, c.Value)
		}
		if flat {
			return html.EscapeString(strings.Join(parts, ", "))
		}
	}
	raw, err := yaml.Marshal(n)
	if err != nil {
		return ""
	}
	return "<code>" + html.EscapeString(strings.TrimSpace(string(raw))) + "</code>"
}
