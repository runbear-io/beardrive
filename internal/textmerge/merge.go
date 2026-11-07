// Package textmerge is the line-based three-way merge shared by the sync
// engine (two devices' concurrent puts) and the hub (an outside write landing
// under a live co-editing room).
//
// Hand-written, no dependency: the code that decides what a merged file
// contains should be readable in one sitting. Line-based on purpose. Word- or
// character-level merging resolves more cases and silently produces sentences
// neither author wrote; a line is the unit people actually edit, and anything
// overlapping is left to the caller's conflict path.
package textmerge

import (
	"bytes"
	"unicode/utf8"
)

// IsText reports whether b is something a line merge may touch: valid UTF-8
// with no NUL. Merging bytes that are not lines produces a file that is
// neither side's, and for a binary format that means a corrupt one.
func IsText(b []byte) bool {
	return !bytes.ContainsRune(b, 0) && utf8.Valid(b)
}

// Merge merges two edits of the same base. ok is false when the two sides
// changed the same region, which is a real conflict and stays one.
//
// Determinism matters more than cleverness here: two devices can each observe
// the same concurrency and merge independently, so merge(base, a, b) and
// merge(base, b, a) must agree or the fleet diverges. Only NON-overlapping
// hunks are ever taken, which makes the result the union either way.
func Merge(base, a, b []byte) ([]byte, bool) {
	ls, la, lb := splitLines(base), splitLines(a), splitLines(b)
	ca, cb := changedRegions(ls, la), changedRegions(ls, lb)
	if overlaps(ca, cb) {
		return nil, false
	}

	// Walk the base once, emitting whichever side replaced each region.
	type edit struct {
		r    region
		with [][]byte
	}
	var edits []edit
	for _, r := range ca {
		edits = append(edits, edit{r, la[r.toFrom:r.toTo]})
	}
	for _, r := range cb {
		edits = append(edits, edit{r, lb[r.toFrom:r.toTo]})
	}
	// Sorted by base position. Disjoint regions can still START at one line:
	// an insertion before line k and the other side replacing from line k.
	// The insertion goes first, whichever side made it — taken the other
	// way round the walk would step back to k and re-emit the replaced
	// lines, and merge(base, a, b) would differ from merge(base, b, a).
	before := func(x, y region) bool {
		if x.from != y.from {
			return x.from < y.from
		}
		return x.to < y.to
	}
	for i := 1; i < len(edits); i++ {
		for j := i; j > 0 && before(edits[j].r, edits[j-1].r); j-- {
			edits[j], edits[j-1] = edits[j-1], edits[j]
		}
	}

	var out [][]byte
	at := 0
	for _, e := range edits {
		for ; at < e.r.from; at++ {
			out = append(out, ls[at])
		}
		out = append(out, e.with...)
		at = e.r.to
	}
	for ; at < len(ls); at++ {
		out = append(out, ls[at])
	}
	return joinLines(out, trailingNewline(a) || trailingNewline(b)), true
}

// region is a half-open span of BASE lines one side replaced, plus where the
// replacement lives in that side.
type region struct {
	from, to     int // in base
	toFrom, toTo int // in the edited side
}

// changedRegions is the coarsest useful diff: trim the common prefix and
// suffix and call the rest one changed span. That is exactly what makes the
// merge safe to reason about — a side "changed" one contiguous region, and
// two sides that changed different regions cannot interfere.
//
// It resolves the case that matters (two people working in different parts of
// a document) and declines everything else to the conflict copy, rather than
// resolving more cases with a hunk matcher nobody can audit.
func changedRegions(base, side [][]byte) []region {
	p := 0
	for p < len(base) && p < len(side) && bytes.Equal(base[p], side[p]) {
		p++
	}
	if p == len(base) && p == len(side) {
		return nil // identical
	}
	s := 0
	for s < len(base)-p && s < len(side)-p &&
		bytes.Equal(base[len(base)-1-s], side[len(side)-1-s]) {
		s++
	}
	return []region{{from: p, to: len(base) - s, toFrom: p, toTo: len(side) - s}}
}

// overlaps reports whether the two sides touched the same base lines. Regions
// that merely ABUT do not overlap — one side replacing lines 1-3 and the other
// 3-5 do, because line 3 is in both.
func overlaps(a, b []region) bool {
	if len(a) == 0 || len(b) == 0 {
		return false // one side did not change anything to conflict with
	}
	for _, x := range a {
		for _, y := range b {
			if x.from < y.to && y.from < x.to {
				return true
			}
			// A pure insertion is an empty base span; two insertions at the
			// same point are ordered by nothing and must not be merged.
			if x.from == x.to && y.from == y.to && x.from == y.from {
				return true
			}
		}
	}
	return false
}

func splitLines(b []byte) [][]byte {
	if len(b) == 0 {
		return nil
	}
	out := bytes.Split(b, []byte("\n"))
	if len(out) > 0 && len(out[len(out)-1]) == 0 {
		out = out[:len(out)-1] // a trailing newline ends the last line
	}
	return out
}

func joinLines(lines [][]byte, trailing bool) []byte {
	out := bytes.Join(lines, []byte("\n"))
	if trailing && len(out) > 0 {
		out = append(out, '\n')
	}
	return out
}

func trailingNewline(b []byte) bool { return len(b) > 0 && b[len(b)-1] == '\n' }
