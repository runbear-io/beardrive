package webapp

import (
	"bytes"
	"net/url"
	"regexp"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
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
// the source is escaped by goldmark's safe default.
func RenderMarkdown(src []byte) (string, error) {
	var buf bytes.Buffer
	if err := md.Convert(expandWikilinks(src), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}
