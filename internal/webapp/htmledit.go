package webapp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

/* Click-to-edit: marking up a served HTML file so the browser can tell which
   text on the rendered page came literally from the file, and where in the
   file it lives.

   The whole feature rests on one promise: an edit replaces ONE element's inner
   byte range in the source and leaves every other byte alone. That promise is
   only keepable if the range is measured against the stored bytes, so every
   offset here is recorded BEFORE any rewriting shifts anything — what this
   emits is a view, never something that gets stored.

   Text a page's own JavaScript produces has no range and therefore no marker,
   so it is simply not clickable. That is the rule falling out of the design
   rather than a heuristic anyone has to maintain.

   The posture throughout is that a MISSING marker is free and a WRONG one is
   damage: a missing marker means "not editable here", while a wrong one means
   an edit splicing over bytes belonging to something else. Every ambiguity
   below therefore resolves to not stamping.

   Nothing here decides permission. serveBlob settles that before calling in. */

// srcAttr carries an element's inner content range in the original source.
// Its presence is what makes a region editable in the browser.
const srcAttr = "data-bd-src"

// maxEditableHTML caps what the editable view will buffer. Stamping cannot
// stream — an element is only known to be innermost once its end tag arrives,
// by which point its start tag is long gone — so the whole document is held in
// memory. The plain render stays streaming and uncapped; this ceiling costs a
// reader nothing and only ever declines to offer editing.
const maxEditableHTML = 4 << 20

// editScriptURL is where the browser fetches the click-to-edit bootstrap. It is
// a separate bundle from the app: a reader never downloads an editor, and a
// sandboxed opaque-origin iframe can still load a classic script cross-origin
// without CORS, so the sandbox stays exactly as tight as it was.
//
// At the static ROOT rather than under assets/, for the reason share-mermaid.js
// is: this is a const string and cannot carry a content hash, and assets/ is
// served immutable for a year — an unhashed name there would pin a stale
// bundle in shared caches. Its own code-split chunks keep hashed assets/ names.
const editScriptURL = "/inline-edit.js"

var errEditTooLarge = errors.New("file too large for the editable view")

// voidElements never hold text, so they are never pushed on the stack. A bare
// <br> or <img> has no end tag, and treating it as open would swallow every
// sibling after it into a phantom element.
var voidElements = map[atom.Atom]bool{
	atom.Area: true, atom.Base: true, atom.Br: true, atom.Col: true,
	atom.Embed: true, atom.Hr: true, atom.Img: true, atom.Input: true,
	atom.Link: true, atom.Meta: true, atom.Param: true, atom.Source: true,
	atom.Track: true, atom.Wbr: true,
}

// inlineElements are phrasing content: markup INSIDE a run of prose rather than
// a container of it. They are never editable regions of their own, and — the
// part that matters — they do not stop their parent from being one.
//
// Without this the innermost rule eats itself. "See the <a>notes</a> for the
// method" would make the <a> the innermost text-bearing element, stamp that,
// and mark the <p> as already-covered — so the only editable region in the
// paragraph is the link text, and the prose around it cannot be touched. Real
// prose almost always contains a link or an emphasis, so almost nothing would
// have been editable.
//
// Instead an inline element hands its text up to its parent and disappears.
// The paragraph is stamped whole, its inner markup goes to the editor intact,
// and whether that markup survives is the round-trip check's business — which
// is exactly where that question belongs: <a> and <strong> are in the schema
// and pass, <span class="badge"> is not and refuses.
var inlineElements = map[atom.Atom]bool{
	atom.A: true, atom.Abbr: true, atom.B: true, atom.Bdi: true, atom.Bdo: true,
	atom.Cite: true, atom.Code: true, atom.Data: true, atom.Dfn: true,
	atom.Del: true, atom.Em: true, atom.I: true, atom.Ins: true, atom.Kbd: true,
	atom.Mark: true, atom.Q: true, atom.Rp: true, atom.Rt: true, atom.Ruby: true,
	atom.S: true, atom.Samp: true, atom.Small: true, atom.Span: true,
	atom.Strong: true, atom.Sub: true, atom.Sup: true, atom.Time: true,
	atom.U: true, atom.Var: true, atom.Big: true, atom.Strike: true,
	atom.Tt: true, atom.Font: true, atom.Nobr: true, atom.Label: true,
}

// neverEditable are the contexts where source text is not prose. script/style
// are code; head/title are metadata; template is inert; svg has its own content
// model. pre and textarea are excluded HERE rather than left to the browser's
// round-trip check because whitespace in them is load-bearing and a paragraph
// schema flattens it — a check that runs after the damage is already
// expressible is a check that runs too late.
var neverEditable = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Head: true, atom.Title: true,
	atom.Template: true, atom.Pre: true, atom.Textarea: true, atom.Svg: true,
}

// impliedBy maps an element to the start tags whose arrival ends it. HTML lets
// <p>one<p>two stand for two siblings, and a tokenizer — unlike a parser — will
// not fix that up. Without these rules the stack reads the second <p> as nested
// inside the first, and the first's range runs to the wrong place.
//
// Being a key here also means "this element's end tag is optional", which is
// what lets a force-close by an ancestor's end tag (<div><p>hi</div>) still
// produce a trustworthy range. Elements absent from this map that turn up
// unclosed are dropped instead.
var impliedBy = map[atom.Atom]map[atom.Atom]bool{
	atom.P: {
		atom.P: true, atom.Div: true, atom.Ul: true, atom.Ol: true,
		atom.Table: true, atom.Section: true, atom.Article: true,
		atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true,
		atom.H5: true, atom.H6: true, atom.Blockquote: true, atom.Pre: true,
		atom.Header: true, atom.Footer: true, atom.Nav: true, atom.Aside: true,
		atom.Form: true, atom.Hr: true, atom.Dl: true, atom.Figure: true,
	},
	atom.Li:     {atom.Li: true},
	atom.Dt:     {atom.Dt: true, atom.Dd: true},
	atom.Dd:     {atom.Dt: true, atom.Dd: true},
	atom.Td:     {atom.Td: true, atom.Th: true, atom.Tr: true},
	atom.Th:     {atom.Td: true, atom.Th: true, atom.Tr: true},
	atom.Tr:     {atom.Tr: true},
	atom.Option: {atom.Option: true, atom.Optgroup: true},
}

// openElem is one entry on the tokenizer's element stack.
type openElem struct {
	tag atom.Atom
	// name is kept alongside tag because atom.Atom is zero for every element
	// outside HTML's known set — a custom element, say — and two different
	// unknown tags would otherwise compare equal and close each other.
	name string
	// chunk indexes the output slice holding this element's start tag, so the
	// decision to stamp — which can only be made at the end tag — can reach
	// back and rewrite it.
	chunk int
	// inner is where this element's content begins in the ORIGINAL source: the
	// offset just past its start tag.
	inner int
	// token is retained so a stamped start tag can be rebuilt rather than
	// string-patched. Rebuilding is also what drops any data-bd-src the file
	// itself contained, which would otherwise offer the browser a range this
	// code never measured.
	token html.Token
	// hasText records a non-whitespace text token directly inside.
	hasText bool
	// hasStamped records that something below was stamped, which is what makes
	// this element not innermost.
	hasStamped bool
	// inert is set when this element, or any ancestor, is a context where
	// source text is not prose.
	inert bool
}

// stamper walks the token stream once, holding the output chunks and the
// element stack. Its only externally interesting output is Result.
type stamper struct {
	out   [][]byte
	stack []openElem
}

/*
utf16Len counts a fragment's length the way JavaScript measures a string.

	Offsets leave here as UTF-16 code-unit counts, not byte counts, and the
	difference is not cosmetic. The browser resolves these against a Y.Text,
	which — like every JS string — is indexed in UTF-16 units. A file that is
	pure ASCII makes the two identical, which is exactly why this is so easy to
	get wrong and so hard to notice: every English fixture passes. Put one
	accented character earlier in the document and a byte offset points into the
	middle of somebody else's markup, and the edit is written there.

	Invalid UTF-8 decodes to one replacement character per bad byte, which is
	what the browser's own decoder produces for the same input.
*/
func utf16Len(b []byte) int {
	n := 0
	for _, r := range string(b) {
		n++
		if r > 0xFFFF { // astral planes are a surrogate pair in UTF-16
			n++
		}
	}
	return n
}

// pop removes the top element, stamping it when it is the innermost holder of
// real text. contentEnd is where its content ends in the original source: the
// offset at which whatever closed it begins, whether that was its own end tag,
// a sibling's start tag, or an ancestor's end tag.
//
// stampable says whether this particular close is trustworthy. A force-close of
// an element whose end tag is not optional means the document's nesting and
// this stack have diverged, and the range would be a guess.
func (s *stamper) pop(contentEnd int, stampable bool) {
	e := s.stack[len(s.stack)-1]
	s.stack = s.stack[:len(s.stack)-1]

	// Phrasing content is part of the prose around it, not a region of its own.
	// Its text belongs to whatever block encloses it, and it leaves no trace
	// that would stop that block being stamped.
	if inlineElements[e.tag] {
		if len(s.stack) > 0 {
			p := &s.stack[len(s.stack)-1]
			p.hasText = p.hasText || e.hasText
			p.hasStamped = p.hasStamped || e.hasStamped
		}
		return
	}

	// The innermost BLOCK directly holding text is the editable unit. Stamping
	// an ancestor as well would hand the browser two overlapping ranges over
	// the same bytes.
	stamp := stampable && e.hasText && !e.hasStamped && !e.inert && contentEnd >= e.inner
	if stamp {
		tok := e.token
		tok.Attr = append(append([]html.Attribute(nil), tok.Attr...),
			html.Attribute{Key: srcAttr, Val: fmt.Sprintf("%d,%d", e.inner, contentEnd)})
		s.out[e.chunk] = []byte(tok.String())
	}
	if (stamp || e.hasStamped) && len(s.stack) > 0 {
		s.stack[len(s.stack)-1].hasStamped = true
	}
}

// closeImplied pops every element that this start tag ends, so <p>one<p>two is
// measured as two siblings — which is what the browser renders — rather than as
// one element nested inside another. at is where the new start tag begins, and
// therefore where the popped element's content stops.
func (s *stamper) closeImplied(opening atom.Atom, at int) {
	for len(s.stack) > 0 && impliedBy[s.stack[len(s.stack)-1].tag][opening] {
		s.pop(at, true)
	}
}

// findOpen locates the innermost open element with this tag name.
func (s *stamper) findOpen(name string) int {
	for i := len(s.stack) - 1; i >= 0; i-- {
		if strings.EqualFold(s.stack[i].name, name) {
			return i
		}
	}
	return -1
}

// stampEditable rewrites HTML source so every innermost text-bearing element
// carries its inner content's byte range in the original source, and appends
// the bootstrap that turns those ranges into editable regions.
//
// It is total: any input returns valid HTML. Malformed markup loses markers,
// never gains wrong ones.
func stampEditable(src []byte, scriptURL string) []byte {
	s := &stamper{}
	z := html.NewTokenizer(bytes.NewReader(src))
	// off is how far into the ORIGINAL source the tokenizer has consumed,
	// counted in UTF-16 code units because that is what the browser will
	// resolve these offsets against (see utf16Len). It advances by the raw
	// token length no matter what gets emitted in its place.
	off := 0

	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break // including io.EOF: whatever was emitted so far stands
		}
		raw := z.Raw()
		start := off
		off += utf16Len(raw)
		// Raw points into the tokenizer's own buffer, which it reuses.
		chunk := append([]byte(nil), raw...)

		switch tt {
		case html.TextToken:
			if len(bytes.TrimSpace(chunk)) > 0 && len(s.stack) > 0 {
				s.stack[len(s.stack)-1].hasText = true
			}
			s.out = append(s.out, chunk)

		case html.StartTagToken:
			tok := z.Token()
			if hasSrcAttr(tok) {
				// The file carries the marker name itself. Rebuild without it,
				// so the browser never sees a range this code did not measure.
				tok = dropSrcAttr(tok)
				chunk = []byte(tok.String())
			}
			if voidElements[tok.DataAtom] {
				s.out = append(s.out, chunk)
				continue
			}
			s.closeImplied(tok.DataAtom, start)
			s.out = append(s.out, chunk)
			inert := neverEditable[tok.DataAtom] ||
				(len(s.stack) > 0 && s.stack[len(s.stack)-1].inert)
			s.stack = append(s.stack, openElem{
				tag: tok.DataAtom, name: tok.Data, chunk: len(s.out) - 1,
				inner: off, token: tok, inert: inert,
			})

		case html.EndTagToken:
			name, _ := z.TagName()
			i := s.findOpen(string(name))
			if i < 0 {
				// An end tag with nothing open to match it. The browser
				// discards it too, and the stack stays consistent.
				s.out = append(s.out, chunk)
				continue
			}
			// Everything above i was left unclosed. Those are trustworthy only
			// where HTML makes the end tag optional; the rest are the
			// adoption-agency cases, where this stack and the browser's tree
			// have genuinely diverged.
			for len(s.stack) > i+1 {
				s.pop(start, impliedBy[s.stack[len(s.stack)-1].tag] != nil)
			}
			s.pop(start, true)
			s.out = append(s.out, chunk)

		default: // self-closing, comments, doctype: pass through untouched
			s.out = append(s.out, chunk)
		}
	}

	sum := sha256.Sum256(src)
	body := bytes.Join(s.out, nil)
	// Appended after the stored bytes — the same place printSuffix goes, and
	// for the same reason: HTML's parser accepts trailing content, and this
	// path never touches what is served for download.
	//
	// The length rides along with the hash because the browser cannot always
	// compute a hash: crypto.subtle exists only in a secure context, and a
	// LAN hub served over plain HTTP is not one. Comparing lengths is a weaker
	// check — it misses an edit that happens to preserve length — but it is
	// the difference between a safety net and none at all on those hubs.
	return fmt.Appendf(body, "\n<script src=%q data-src-hash=%q data-src-len=%q defer></script>",
		scriptURL, hex.EncodeToString(sum[:]), fmt.Sprint(utf16Len(src)))
}

func hasSrcAttr(t html.Token) bool {
	for _, a := range t.Attr {
		if strings.EqualFold(a.Key, srcAttr) {
			return true
		}
	}
	return false
}

func dropSrcAttr(t html.Token) html.Token {
	kept := make([]html.Attribute, 0, len(t.Attr))
	for _, a := range t.Attr {
		if !strings.EqualFold(a.Key, srcAttr) {
			kept = append(kept, a)
		}
	}
	t.Attr = kept
	return t
}

// editView reports whether this request asked for the editable variant of an
// inline render. It answers only "was this asked for, and is it possible at
// all"; whether the caller MAY edit is settled before this is reached, by the
// same writable-path check the save door runs.
//
// text/html only, and never on a download: an attachment is saved rather than
// rendered, and stamping it would put markers in a file on someone's disk.
func editView(r *http.Request, ct string, inline bool) bool {
	if !inline || r.URL.Query().Get("edit") != "1" {
		return false
	}
	return strings.HasPrefix(strings.ToLower(ct), "text/html")
}

// readCapped reads up to max bytes, reporting errEditTooLarge rather than
// returning a truncated document — half a file would stamp ranges running off
// the end of what the browser was given.
//
// On errEditTooLarge the bytes read so far come back ALONGSIDE the error: the
// caller is mid-stream through someone's file and needs them to serve it whole.
func readCapped(r io.Reader, max int) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return b, err
	}
	if len(b) > max {
		return b, errEditTooLarge
	}
	return b, nil
}
