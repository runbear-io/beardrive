// Package docrefs is the one definition of an "outgrown" doc: a markdown file
// that references a file written after the doc itself. `bdrive stale`, the
// hub's file page and the agent hook all call it, so the three can never
// disagree about what stale means.
package docrefs

import (
	"bufio"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// MaxLineScan bounds one line, so a minified bundle that syncs cannot make a
// line scanner buffer the whole file.
const MaxLineScan = 1 << 20

// linkRe matches a markdown inline link's target: [label](target).
var linkRe = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)`)

// wikiRe matches Obsidian-style [[target]] and [[target|label]] links.
// Copied from internal/webapp/markdown.go rather than shared: the renderer's
// copy is part of its own parsing, this one only harvests targets.
var wikiRe = regexp.MustCompile(`\[\[([^\]|]+)(?:\|([^\]]+))?\]\]`)

// pathRe matches a bare path-shaped token — at least one slash, and no
// wrapping punctuation, so a backticked `cmd/bdrive/grep.go` yields the path
// and not the backticks. Resolution is the real filter, so this stays loose.
var pathRe = regexp.MustCompile(`[A-Za-z0-9._~@+-]+(?:/[A-Za-z0-9._~@+-]+)+`)

// schemeRe matches a URL scheme, so https:// and mailto: never resolve.
var schemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// Ref is one reference that has outrun its doc.
type Ref struct {
	Path string
	Time time.Time
	Gap  time.Duration
}

// IsMarkdown reports whether a path is a doc this package reads.
func IsMarkdown(rel string) bool {
	switch strings.ToLower(path.Ext(rel)) {
	case ".md", ".markdown":
		return true
	}
	return false
}

// Candidates harvests every reference-shaped string in a doc, unresolved and
// in document order. An over-long line ends the doc, never the caller.
func Candidates(r io.Reader) []string {
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLineScan)
	for sc.Scan() {
		line := sc.Text()
		for _, m := range linkRe.FindAllStringSubmatch(line, -1) {
			out = append(out, m[1])
		}
		for _, m := range wikiRe.FindAllStringSubmatch(line, -1) {
			// A wikilink names a doc, usually without its extension.
			out = append(out, m[1], m[1]+".md")
		}
		out = append(out, pathRe.FindAllString(line, -1)...)
	}
	return out
}

// Resolve turns a doc's candidates into the paths they name, deduped, in
// candidate order, never the doc itself. exists is the set of files the
// caller syncs: resolution IS the filter, so a loose extractor costs nothing.
func Resolve(docRel string, cands []string, exists func(string) bool) []string {
	docDir := path.Dir(docRel)
	seen := map[string]bool{}
	var refs []string
	for _, c := range cands {
		target, ok := resolve(docDir, c, exists)
		if !ok || target == docRel || seen[target] {
			continue
		}
		seen[target] = true
		refs = append(refs, target)
	}
	return refs
}

func resolve(docDir, cand string, exists func(string) bool) (string, bool) {
	cand = strings.TrimSpace(cand)
	// A trailing anchor or query is not part of the path.
	if i := strings.IndexAny(cand, "#?"); i >= 0 {
		cand = cand[:i]
	}
	cand = strings.TrimRight(cand, `.,;:!?"'`)
	if cand == "" || strings.HasPrefix(cand, "/") || schemeRe.MatchString(cand) {
		return "", false // absolute, protocol-relative (//host), or a URL
	}
	tries := []string{path.Clean(cand)}
	if docDir != "." {
		tries = append([]string{path.Join(docDir, cand)}, tries...)
	}
	for _, p := range tries {
		// Never leave the mount, and never name the root itself.
		if p == "." || p == "/" || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
			continue
		}
		if exists(p) {
			return p, true
		}
	}
	return "", false
}

// WriteTimes dates every path from the journal, newest write wins.
//
// Max by DisplayTime, not the newest op under journal.Less: DisplayTime is
// what `bdrive log` sorts by, and it returns the zero time for an op stamped
// in the future — so taking the causally-newest op would date that path to
// year 1 and flag every doc referencing it. Max discards the zero naturally.
func WriteTimes(ops []journal.Op) map[string]time.Time {
	written := make(map[string]time.Time, len(ops))
	for _, op := range ops {
		if t := WriteTime(op); !t.IsZero() {
			if cur, ok := written[op.Path]; !ok || t.After(cur) {
				written[op.Path] = t
			}
		}
	}
	return written
}

// WriteTime is one op's contribution to its path's write time: zero for a
// delete or an op we cannot date, which never gets to date a path.
func WriteTime(op journal.Op) time.Time {
	if op.Kind != journal.KindPut {
		return time.Time{}
	}
	return journal.DisplayTime(op)
}

// Outgrown returns the refs written after docTime, worst gap first.
func Outgrown(docTime time.Time, refs []string, written func(string) (time.Time, bool)) []Ref {
	var out []Ref
	for _, p := range refs {
		t, ok := written(p)
		if !ok || !t.After(docTime) {
			continue
		}
		out = append(out, Ref{Path: p, Time: t, Gap: t.Sub(docTime)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Gap > out[j].Gap })
	return out
}
