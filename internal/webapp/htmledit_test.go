package webapp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// stampedRanges parses stamped output and returns, per marked element, the
// range it claims and the text the browser will actually show inside it.
type stamped struct {
	tag        string
	start, end int
	text       string // normalized text content as parsed
}

func parseStamped(t *testing.T, out []byte) []stamped {
	t.Helper()
	doc, err := html.Parse(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("stamped output does not parse: %v", err)
	}
	var found []stamped
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if a.Key != srcAttr {
					continue
				}
				lo, hi, ok := strings.Cut(a.Val, ",")
				if !ok {
					t.Fatalf("%s=%q is not a range", srcAttr, a.Val)
				}
				s, err1 := strconv.Atoi(lo)
				e, err2 := strconv.Atoi(hi)
				if err1 != nil || err2 != nil {
					t.Fatalf("%s=%q is not numeric", srcAttr, a.Val)
				}
				found = append(found, stamped{tag: n.Data, start: s, end: e, text: nodeText(n)})
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

/*
sliceU16 takes the substring a BROWSER would take for these offsets.

	The offsets are UTF-16 code-unit counts, because that is how a JS string —
	and so a Y.Text — is indexed. Slicing the Go []byte directly would be right
	only for ASCII, which is precisely the trap this whole area sets: every
	English fixture agrees, and the first accented character silently disagrees.
	Tests have to measure the same way the client does or they prove nothing.
*/
func sliceU16(src []byte, start, end int) string {
	var b strings.Builder
	n := 0
	for _, r := range string(src) {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if n >= start && n+w <= end {
			b.WriteRune(r)
		}
		n += w
	}
	return b.String()
}

var wsRun = regexp.MustCompile(`\s+`)

func normText(s string) string {
	return strings.TrimSpace(wsRun.ReplaceAllString(s, " "))
}

// sliceText renders the text a raw source fragment would show, so a claimed
// range can be compared against what the browser put on screen.
func sliceText(frag string) string {
	doc, err := html.Parse(strings.NewReader(frag))
	if err != nil {
		return ""
	}
	return nodeText(doc)
}

// TestStampRangesMatchSource is the invariant the whole feature rests on: a
// stamped range must delimit exactly the bytes of that element's content in the
// ORIGINAL source. An off-by-one here is an edit that eats a neighbour's markup.
func TestStampRangesMatchSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"simple", `<html><body><p>Hello world</p></body></html>`},
		{"two paragraphs", `<p>one</p>
<p>two</p>`},
		{"nested inline", `<p>make <strong>this</strong> bold</p>`},
		{"list", `<ul><li>alpha</li><li>beta</li></ul>`},
		{"table", `<table><tr><td>a</td><td>b</td></tr></table>`},
		{"heading and prose", `<h1>Title</h1><div class="x"><p>Body text here.</p></div>`},
		{"attributes preserved", `<p class="lead" data-n="3">Hi</p>`},
		{"entities", `<p>a &amp; b &lt; c</p>`},
		{"comment between", `<p>one</p><!-- note --><p>two</p>`},
		{"implied close p", `<div><p>one<p>two</div>`},
		{"implied close li", `<ul><li>one<li>two</ul>`},
		{"unclosed p before div", `<p>text<div>other</div>`},
		{"indented", "<body>\n  <p>\n    padded\n  </p>\n</body>"},
		{"link inside", `<p>see <a href="/x?a=1&amp;b=2">here</a> now</p>`},
		{"figure caption", `<figure><img src="a.png"><figcaption>Cap</figcaption></figure>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(tc.src)
			out := stampEditable(src, "/x.js")
			marks := parseStamped(t, out)
			if len(marks) == 0 {
				t.Fatalf("nothing stamped in %q", tc.src)
			}
			for _, m := range marks {
				if m.start < 0 || m.end > utf16Len(src) || m.start > m.end {
					t.Fatalf("%s range %d,%d out of bounds for %d units",
						m.tag, m.start, m.end, utf16Len(src))
				}
				frag := sliceU16(src, m.start, m.end)
				got := normText(sliceText(frag))
				want := normText(m.text)
				if got != want {
					t.Errorf("<%s> range %d,%d slices to %q, but the element shows %q\n  slice = %q",
						m.tag, m.start, m.end, got, want, frag)
				}
			}
		})
	}
}

/*
Offsets are UTF-16 code units, not bytes, and this is the test that can tell.

	Every other fixture in this file is ASCII, where the two counts are equal —
	so all of them passed while the server was emitting byte offsets and the
	browser was resolving them as JS string indices. One accented character
	earlier in the document is enough to desynchronise them, and the symptom is
	not a crash: it is an edit written over the middle of somebody else's
	markup, in a file that belongs to a teammate.

	Any language that is not English hits this on the first paragraph.
*/
func TestStampRangesAreUTF16Offsets(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		// 'é' is 2 bytes / 1 UTF-16 unit — the common case.
		{"accents", `<p>Café</p><p>Chargé d'affaires</p>`},
		// Each CJK char is 3 bytes / 1 unit.
		{"cjk", `<h1>四半期レポート</h1><p>売上は好調です。</p>`},
		// An emoji is 4 bytes / 2 units — the case that also breaks a naive
		// rune count, which is the obvious wrong fix for the byte-count bug.
		{"emoji", `<p>Shipped 🚀 today</p><p>Second 🎉 one</p>`},
		{"mixed", `<p>Héllo 🌍</p><ul><li>日本語</li><li>plain</li></ul>`},
		{"non-ascii before the target", `<p>Ünicode</p><p>the edited one</p>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(tc.src)
			marks := parseStamped(t, stampEditable(src, "/x.js"))
			if len(marks) == 0 {
				t.Fatalf("nothing stamped in %q", tc.src)
			}
			for _, m := range marks {
				// Sliced the way the BROWSER will slice: by UTF-16 units.
				got := normText(sliceText(sliceU16(src, m.start, m.end)))
				if want := normText(m.text); got != want {
					t.Errorf("<%s> range %d,%d slices to %q, want %q\n"+
						"  (byte-sliced it would be %q — the bug this catches)",
						m.tag, m.start, m.end, got, want,
						safeByteSlice(src, m.start, m.end))
				}
			}
		})
	}
}

// safeByteSlice is only for the failure message above: it shows what a byte
// interpretation of the same offsets would have grabbed.
func safeByteSlice(src []byte, start, end int) string {
	if start < 0 || end > len(src) || start > end {
		return "<out of range>"
	}
	return string(src[start:end])
}

// TestStampRangesDoNotOverlap pins the innermost rule from the other side: two
// overlapping ranges would let one edit silently clobber another's bytes.
func TestStampRangesDoNotOverlap(t *testing.T) {
	src := []byte(`<div><p>one</p><ul><li>a</li><li>b</li></ul><blockquote><p>q</p></blockquote></div>`)
	marks := parseStamped(t, stampEditable(src, "/x.js"))
	for i := range marks {
		for j := i + 1; j < len(marks); j++ {
			a, b := marks[i], marks[j]
			if a.start < b.end && b.start < a.end {
				t.Errorf("<%s>(%d,%d) overlaps <%s>(%d,%d)",
					a.tag, a.start, a.end, b.tag, b.start, b.end)
			}
		}
	}
}

func stampedTags(t *testing.T, src string) []string {
	t.Helper()
	var tags []string
	for _, m := range parseStamped(t, stampEditable([]byte(src), "/x.js")) {
		tags = append(tags, m.tag)
	}
	return tags
}

// TestStampExclusions holds the line on contexts where source text is not prose.
// script and style are the ones that would be actively dangerous to hand to a
// paragraph editor; pre and textarea are the ones where whitespace is content.
func TestStampExclusions(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"script", `<body><script>var x = "hello";</script></body>`},
		{"style", `<body><style>.a { color: red; }</style></body>`},
		{"title", `<html><head><title>Doc</title></head><body></body></html>`},
		{"pre", `<body><pre>  spaced
  lines</pre></body>`},
		{"textarea", `<body><textarea>typed text</textarea></body>`},
		{"template", `<body><template><p>latent</p></template></body>`},
		{"svg text", `<body><svg><text>label</text></svg></body>`},
		{"code in pre", `<body><pre><code>fmt.Println()</code></pre></body>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tags := stampedTags(t, tc.src); len(tags) != 0 {
				t.Errorf("stamped %v inside an excluded context: %s", tags, tc.src)
			}
		})
	}
}

// TestStampInnermostOnly: a container whose text lives in children is not
// itself editable, or the browser gets two ranges over the same bytes.
func TestStampInnermostOnly(t *testing.T) {
	src := `<html><body><div><section><p>deep text</p></section></div></body></html>`
	tags := stampedTags(t, src)
	if len(tags) != 1 || tags[0] != "p" {
		t.Fatalf("want exactly [p], got %v", tags)
	}
}

// Inline markup is part of the prose, not a region beside it.
//
// This is the rule the innermost test above cannot see, and getting it wrong is
// almost invisible: every paragraph of PLAIN text still works, so the feature
// looks fine until somebody tries to fix a typo in a sentence that happens to
// contain a link — and finds the only editable thing in it is the link text.
// Real prose nearly always contains an <a> or an <em>, so this is the
// difference between a working editor and a demo.
func TestStampTreatsInlineMarkupAsContent(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"link mid-sentence", `<p>See the <a href="/x">notes</a> for detail.</p>`, []string{"p"}},
		{"emphasis", `<p>This is <strong>important</strong> today.</p>`, []string{"p"}},
		{"nested inline", `<p>a <em>b <code>c</code></em> d</p>`, []string{"p"}},
		{"span", `<p>Margin was <span class="badge">up</span> again.</p>`, []string{"p"}},
		{"list item with a link", `<ul><li>Go <a href="/y">here</a>.</li></ul>`, []string{"li"}},
		// A link that is the element's ENTIRE content is still the paragraph's
		// content — the editable unit is the block either way.
		{"link is everything", `<p><a href="/x">only a link</a></p>`, []string{"p"}},
		// Two blocks, each whole.
		{"siblings", `<p>one <b>x</b></p><p>two <i>y</i></p>`, []string{"p", "p"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := stampedTags(t, tc.src)
			if len(got) != len(tc.want) {
				t.Fatalf("stamped %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("stamped %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// The corollary, stated from the file's side: the range handed to the browser
// must cover the inline markup, not stop at it. An edit that replaced only the
// text either side of a link would delete the link.
func TestStampRangeCoversInlineMarkup(t *testing.T) {
	src := []byte(`<p>See the <a href="/x">notes</a> for detail.</p>`)
	marks := parseStamped(t, stampEditable(src, "/x.js"))
	if len(marks) != 1 {
		t.Fatalf("want 1 mark, got %d: %+v", len(marks), marks)
	}
	got := sliceU16(src, marks[0].start, marks[0].end)
	if want := `See the <a href="/x">notes</a> for detail.`; got != want {
		t.Errorf("range slices to %q, want %q", got, want)
	}
}

// TestStampMixedContent: an element holding BOTH loose text and a child block
// is a genuine ambiguity — a range over it would swallow the child's markup.
// Whatever it does, it must not produce a range that overlaps the child's.
func TestStampMixedContent(t *testing.T) {
	src := []byte(`<div>loose text<p>inner</p></div>`)
	marks := parseStamped(t, stampEditable(src, "/x.js"))
	for i := range marks {
		for j := i + 1; j < len(marks); j++ {
			a, b := marks[i], marks[j]
			if a.start < b.end && b.start < a.end {
				t.Fatalf("mixed content produced overlapping ranges: %+v / %+v", a, b)
			}
		}
	}
	for _, m := range marks {
		got := normText(sliceText(sliceU16(src, m.start, m.end)))
		if want := normText(m.text); got != want {
			t.Errorf("<%s> slices to %q, shows %q", m.tag, got, want)
		}
	}
}

// TestStampMalformedIsSafe: torn markup must lose markers, never gain wrong
// ones. Anything that panics or claims a range outside the file is a bug that
// would corrupt a real document.
func TestStampMalformedIsSafe(t *testing.T) {
	for _, src := range []string{
		`<p>unclosed`,
		`<b><i>overlapping</b></i>`,
		`</p>stray end tag`,
		`<div><p>a</div></p>`,
		`<<>><p>weird</p>`,
		`<p>one<p>two<p>three`,
		`<table><td>no row</td></table>`,
		`<p title="unterminated>text</p>`,
		strings.Repeat(`<div>`, 200) + `deep` + strings.Repeat(`</div>`, 200),
		``,
		`no tags at all`,
		"<p>\x00null byte</p>",
	} {
		t.Run(fmt.Sprintf("%.24q", src), func(t *testing.T) {
			b := []byte(src)
			out := stampEditable(b, "/x.js") // must not panic
			for _, m := range parseStamped(t, out) {
				if m.start < 0 || m.end > utf16Len(b) || m.start > m.end {
					t.Fatalf("bad range %d,%d for %d units of %q", m.start, m.end, utf16Len(b), src)
				}
			}
		})
	}
}

// TestStampDropsForgedMarker: a file containing the marker name itself must not
// be able to hand the browser a range this code never measured.
func TestStampDropsForgedMarker(t *testing.T) {
	src := []byte(`<body><p data-bd-src="0,99999">text</p></body>`)
	marks := parseStamped(t, stampEditable(src, "/x.js"))
	if len(marks) != 1 {
		t.Fatalf("want 1 mark, got %d", len(marks))
	}
	if marks[0].end > utf16Len(src) {
		t.Fatalf("forged range survived: %+v", marks[0])
	}
	if normText(sliceText(sliceU16(src, marks[0].start, marks[0].end))) != "text" {
		t.Errorf("range does not cover the real content: %+v", marks[0])
	}
}

// TestStampHashIsOfStoredBytes: the parent uses this to decide whether the
// ranges it was handed still describe the document it holds. A hash of anything
// other than the stored bytes makes that check meaningless.
func TestStampHashIsOfStoredBytes(t *testing.T) {
	src := []byte(`<p>content</p>`)
	out := string(stampEditable(src, "/assets/inline-edit.js"))
	sum := sha256.Sum256(src)
	if want := `data-src-hash="` + hex.EncodeToString(sum[:]) + `"`; !strings.Contains(out, want) {
		t.Errorf("missing %s in:\n%s", want, out)
	}
	if !strings.Contains(out, `<script src="/assets/inline-edit.js"`) {
		t.Errorf("bootstrap not appended:\n%s", out)
	}
}

// The length travels with the hash for the hubs that cannot compute one:
// crypto.subtle is secure-context only, so a LAN hub on plain HTTP has no
// WebCrypto. It is counted in UTF-16 units like the offsets, because the
// client compares it against a JS string's length.
func TestStampCarriesTheSourceLength(t *testing.T) {
	for _, src := range []string{
		`<p>plain ascii</p>`,
		`<p>Café 🚀</p>`, // 2-byte and 4-byte runes: bytes and units diverge
		`<h1>四半期</h1>`,
	} {
		out := string(stampEditable([]byte(src), "/x.js"))
		want := fmt.Sprintf(`data-src-len=%q`, fmt.Sprint(utf16Len([]byte(src))))
		if !strings.Contains(out, want) {
			t.Errorf("missing %s for %q in:\n%s", want, src, out)
		}
		// The whole point: for non-ASCII this is NOT the byte length.
		if n := utf16Len([]byte(src)); n != len(src) && strings.Contains(out, fmt.Sprintf(`data-src-len="%d"`, len(src))) {
			t.Errorf("length was stamped in bytes (%d), not UTF-16 units (%d)", len(src), n)
		}
	}
}

// TestStampPreservesEverythingElse: the bytes outside a start tag are the file,
// and the editable view is only a view. Comments, doctype, scripts and
// indentation all have to survive verbatim.
func TestStampPreservesEverythingElse(t *testing.T) {
	src := []byte(`<!DOCTYPE html>
<!-- a comment -->
<html>
  <head><style>.a{color:red}</style></head>
  <body>
    <p>Hello</p>
    <script>var x = 1 < 2;</script>
  </body>
</html>`)
	out := stampEditable(src, "/x.js")
	for _, want := range []string{
		"<!DOCTYPE html>", "<!-- a comment -->", ".a{color:red}",
		"var x = 1 < 2;", "\n  <body>\n    ",
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("stamping lost %q", want)
		}
	}
}
