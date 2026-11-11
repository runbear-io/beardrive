// Package textmerge is the line-based three-way merge shared by the sync
// engine (two devices' concurrent puts) and the hub (an outside write landing
// under a live co-editing room).
//
// Hand-written, no dependency: the code that decides what a merged file
// contains should be readable in one sitting. Each side is diffed against the
// base with a Myers O(ND) line diff, so a side that touched several places in
// a file produces several small hunks rather than one span from its first
// change to its last. Hunks that do not touch the same base lines apply
// independently (diff3). Where both sides changed the same lines, three
// narrow cases still resolve without inventing text: an identical edit, two
// insertions at one point of a non-empty file (one already holding the other,
// or both kept in a content-defined order), and a single line both sides
// edited in different words. Everything else is a real conflict and stays one.
package textmerge

import (
	"bytes"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// IsText reports whether b is something a line merge may touch: valid UTF-8
// with no NUL. Merging bytes that are not lines produces a file that is
// neither side's, and for a binary format that means a corrupt one.
func IsText(b []byte) bool {
	return !bytes.ContainsRune(b, 0) && utf8.Valid(b)
}

// Merge merges two edits of the same base. ok is false when the two sides
// changed the same region in different ways, which is a real conflict and
// stays one, or when any input is not text.
//
// Determinism matters more than cleverness here: two devices can each observe
// the same concurrency and merge independently, so merge(base, a, b) and
// merge(base, b, a) must agree or the fleet diverges. Every rule below is
// symmetric in a and b; where an order has to be picked (two insertions at
// one point) it is picked by content, never by argument position.
func Merge(base, a, b []byte) ([]byte, bool) {
	if !IsText(base) || !IsText(a) || !IsText(b) {
		return nil, false
	}
	ls, la, lb := splitLines(base), splitLines(a), splitLines(b)
	out, ok := merge3(ls, la, lb, true)
	if !ok {
		return nil, false
	}
	// The final newline is one more thing a side may change: whichever side
	// changed it from the base wins, and both can only change it one way.
	trailing := trailingNewline(a)
	if trailing == trailingNewline(base) {
		trailing = trailingNewline(b)
	}
	return joinLines(out, trailing), true
}

// region is a half-open span of BASE units one side replaced, plus where the
// replacement lives in that side.
type region struct {
	from, to     int // in base
	toFrom, toTo int // in the edited side
}

// conflicting reports whether two hunks from opposite sides touch the same
// base units. Regions that merely ABUT do not — one side replacing 1-3 and
// the other 3-5 do, because unit 3 is in both. Two pure insertions at the same
// point are ordered by nothing, so they are grouped and resolved explicitly.
func conflicting(x, y region) bool {
	if x.from < y.to && y.from < x.to {
		return true
	}
	return x.from == x.to && y.from == y.to && x.from == y.from
}

// merge3 is diff3 over sequences of units (lines, or tokens of one line).
// lines enables the two line-only resolutions: concatenating same-point
// insertions and merging one line word by word.
func merge3(base, a, b [][]byte, lines bool) ([][]byte, bool) {
	ca, cb := diffRegions(base, a), diffRegions(base, b)
	if len(ca) == 0 {
		return b, true
	}
	if len(cb) == 0 {
		return a, true
	}

	// Group hunks that conflict, transitively (union-find). Both lists are
	// sorted by base position and disjoint within a side, so a sweep finds
	// every conflicting pair without comparing all of them.
	n := len(ca) + len(cb)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	j := 0
	for i, x := range ca {
		for j < len(cb) && cb[j].to < x.from {
			j++
		}
		for k := j; k < len(cb) && cb[k].from <= x.to; k++ {
			if conflicting(x, cb[k]) {
				if ri, rk := find(i), find(len(ca)+k); ri != rk {
					parent[ri] = rk
				}
			}
		}
	}

	type unit struct {
		lo, hi int
		as, bs []region
	}
	units := map[int]*unit{}
	order := []int{}
	add := func(idx int, r region, sideA bool) {
		root := find(idx)
		u := units[root]
		if u == nil {
			u = &unit{lo: r.from, hi: r.to}
			units[root] = u
			order = append(order, root)
		}
		u.lo, u.hi = min(u.lo, r.from), max(u.hi, r.to)
		if sideA {
			u.as = append(u.as, r)
		} else {
			u.bs = append(u.bs, r)
		}
	}
	for i, r := range ca {
		add(i, r, true)
	}
	for k, r := range cb {
		add(len(ca)+k, r, false)
	}
	list := make([]*unit, 0, len(order))
	for _, root := range order {
		list = append(list, units[root])
	}
	// Sorted by base position. Disjoint units can still START at one unit:
	// an insertion before k and the other side replacing from k. The
	// insertion goes first, whichever side made it.
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].lo != list[j].lo {
			return list[i].lo < list[j].lo
		}
		return list[i].hi < list[j].hi
	})

	var out [][]byte
	at := 0
	for _, u := range list {
		for ; at < u.lo; at++ {
			out = append(out, base[at])
		}
		switch {
		case len(u.bs) == 0:
			out = append(out, applyWithin(base, a, u.lo, u.hi, u.as)...)
		case len(u.as) == 0:
			out = append(out, applyWithin(base, b, u.lo, u.hi, u.bs)...)
		default:
			got, ok := resolve(base, a, b, u.lo, u.hi, u.as, u.bs, lines)
			if !ok {
				return nil, false
			}
			out = append(out, got...)
		}
		at = max(at, u.hi)
	}
	for ; at < len(base); at++ {
		out = append(out, base[at])
	}
	return out, true
}

// applyWithin is one side's version of base[lo:hi], with that side's hunks
// inside the span applied.
func applyWithin(base, side [][]byte, lo, hi int, hunks []region) [][]byte {
	var out [][]byte
	at := lo
	for _, h := range hunks {
		out = append(out, base[at:h.from]...)
		out = append(out, side[h.toFrom:h.toTo]...)
		at = h.to
	}
	return append(out, base[at:hi]...)
}

// resolve settles a span both sides changed, or reports a conflict.
func resolve(base, a, b [][]byte, lo, hi int, as, bs []region, lines bool) ([][]byte, bool) {
	spanA, spanB := applyWithin(base, a, lo, hi, as), applyWithin(base, b, lo, hi, bs)
	if equalUnits(spanA, spanB) {
		return spanA, true // the same edit on both sides: emitted once
	}
	if len(as) != 1 || len(bs) != 1 {
		return nil, false
	}
	x, y := as[0], bs[0]
	aRepl, bRepl := a[x.toFrom:x.toTo], b[y.toFrom:y.toTo]
	if x.from == x.to && y.from == y.to {
		// Two different insertions at one point (identical ones were emitted
		// once above). Neither is taken as a retry of the other: a repeated
		// log line appended twice is two lines, and only the caller knows
		// whether a write already landed.
		if !lines {
			return nil, false // two different words at one point: no sentence to make
		}
		if len(base) == 0 {
			// Over an EMPTY ancestor the "one point" is the whole file: two
			// devices each creating it, or filling in a file that was touched
			// empty. One body that begins with the whole other is that file
			// written further; two different bodies are two documents, not
			// two additions to one, and spliced together they read as neither.
			if len(aRepl) <= len(bRepl) && equalUnits(bRepl[:len(aRepl)], aRepl) {
				return bRepl, true
			}
			if len(bRepl) < len(aRepl) && equalUnits(aRepl[:len(bRepl)], bRepl) {
				return aRepl, true
			}
			return nil, false
		}
		// Two people adding at one place: keep both, in an order that does
		// not depend on which side is which.
		first, second := aRepl, bRepl
		if bytes.Compare(bytes.Join(bRepl, []byte("\n")), bytes.Join(aRepl, []byte("\n"))) < 0 {
			first, second = bRepl, aRepl
		}
		out := make([][]byte, 0, len(first)+len(second))
		out = append(out, first...)
		return append(out, second...), true
	}
	if lines && x.from == y.from && x.to == y.to && x.to-x.from == 1 && len(aRepl) == 1 && len(bRepl) == 1 {
		// One line both sides edited: merge it word by word, with the same
		// rule — different words merge, the same word changed two ways is a
		// conflict.
		got, ok := merge3(tokens(base[x.from]), tokens(aRepl[0]), tokens(bRepl[0]), false)
		if !ok {
			return nil, false
		}
		return [][]byte{bytes.Join(got, nil)}, true
	}
	return nil, false
}

// tokens splits one line into maximal runs of whitespace and non-whitespace,
// which join back to exactly the line.
func tokens(line []byte) [][]byte {
	var out [][]byte
	start, prev := 0, -1
	for i, r := range string(line) {
		kind := 0
		if unicode.IsSpace(r) {
			kind = 1
		}
		if prev != -1 && kind != prev {
			out = append(out, line[start:i])
			start = i
		}
		prev = kind
	}
	if start < len(line) {
		out = append(out, line[start:])
	}
	return out
}

func equalUnits(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// maxEditDistance bounds the Myers search, whose trace costs O(D²) memory.
// Past it the changed middle is one region, which is the coarse diff this
// package used before: still correct, only less able to merge.
const maxEditDistance = 2000

// diffRegions is a minimal line diff of side against base: the disjoint
// hunks, each separated from the next by at least one unchanged unit.
func diffRegions(base, side [][]byte) []region {
	n, m := len(base), len(side)
	p := 0
	for p < n && p < m && bytes.Equal(base[p], side[p]) {
		p++
	}
	if p == n && p == m {
		return nil
	}
	s := 0
	for s < n-p && s < m-p && bytes.Equal(base[n-1-s], side[m-1-s]) {
		s++
	}
	whole := []region{{from: p, to: n - s, toFrom: p, toTo: m - s}}
	if n-s == p || m-s == p {
		return whole // pure insertion or pure deletion
	}
	ids := map[string]int{}
	intern := func(units [][]byte) []int {
		out := make([]int, len(units))
		for i, u := range units {
			id, ok := ids[string(u)]
			if !ok {
				id = len(ids)
				ids[string(u)] = id
			}
			out[i] = id
		}
		return out
	}
	x, y := intern(base[p:n-s]), intern(side[p:m-s])
	matches, ok := myers(x, y)
	if !ok {
		return whole
	}
	var out []region
	pi, pj := 0, 0
	for _, mt := range matches {
		if mt[0] > pi || mt[1] > pj {
			out = append(out, region{from: p + pi, to: p + mt[0], toFrom: p + pj, toTo: p + mt[1]})
		}
		pi, pj = mt[0]+1, mt[1]+1
	}
	if pi < len(x) || pj < len(y) {
		out = append(out, region{from: p + pi, to: p + len(x), toFrom: p + pj, toTo: p + len(y)})
	}
	return out
}

// myers returns the matched (a, b) index pairs of a shortest edit script, in
// order, or false when the edit distance exceeds maxEditDistance.
func myers(a, b []int) ([][2]int, bool) {
	n, m := len(a), len(b)
	limit := min(n+m, maxEditDistance)
	off := limit + 1
	v := make([]int, 2*limit+3)
	// trace[d] is v[-d..d] after step d, indexed by k+d.
	var trace [][]int
	for d := 0; d <= limit; d++ {
		done := false
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				done = true
				break
			}
		}
		trace = append(trace, append([]int(nil), v[off-d:off+d+1]...))
		if done {
			return backtrack(trace, n, m), true
		}
	}
	return nil, false
}

func backtrack(trace [][]int, n, m int) [][2]int {
	var rev [][2]int
	x, y := n, m
	for d := len(trace) - 1; d > 0; d-- {
		prev := trace[d-1]
		at := func(k int) int { return prev[k+d-1] }
		k := x - y
		var prevK int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := at(prevK)
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			rev = append(rev, [2]int{x - 1, y - 1})
			x--
			y--
		}
		x, y = prevX, prevY
	}
	for x > 0 && y > 0 {
		rev = append(rev, [2]int{x - 1, y - 1})
		x--
		y--
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// CharHunk is one edit turning old text into new: replace old[Start:End] with
// Insert. Offsets are UTF-16 code units into the OLD text, which is what Yjs
// counts in.
type CharHunk struct {
	Start, End int
	Insert     string
}

// DiffHunks is the minimal set of disjoint edits turning oldText into
// newText, ascending: a Myers line diff, each changed span then trimmed to
// the characters that actually differ. Applied from last to first, each
// edit's offsets stay valid. A splice made this way keeps every unchanged
// line as the same CRDT items instead of re-inserting the whole middle.
func DiffHunks(oldText, newText string) []CharHunk {
	if oldText == newText {
		return nil
	}
	ol, nl := splitKeep(oldText), splitKeep(newText)
	toBytes := func(ss []string) [][]byte {
		out := make([][]byte, len(ss))
		for i, s := range ss {
			out[i] = []byte(s)
		}
		return out
	}
	regions := diffRegions(toBytes(ol), toBytes(nl))
	// UTF-16 offset at the start of each old line.
	starts := make([]int, len(ol)+1)
	for i, l := range ol {
		starts[i+1] = starts[i] + u16len(l)
	}
	out := make([]CharHunk, 0, len(regions))
	for _, r := range regions {
		was := strings.Join(ol[r.from:r.to], "")
		now := strings.Join(nl[r.toFrom:r.toTo], "")
		a, b := []rune(was), []rune(now)
		p := 0
		for p < len(a) && p < len(b) && a[p] == b[p] {
			p++
		}
		s := 0
		for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
			s++
		}
		start := starts[r.from] + len(utf16.Encode(a[:p]))
		del := len(utf16.Encode(a[p : len(a)-s]))
		ins := string(b[p : len(b)-s])
		if del == 0 && ins == "" {
			continue
		}
		out = append(out, CharHunk{Start: start, End: start + del, Insert: ins})
	}
	return out
}

// splitKeep splits text into lines that keep their "\n", so they join back
// to exactly the text.
func splitKeep(s string) []string {
	if s == "" {
		return nil
	}
	out := strings.SplitAfter(s, "\n")
	if out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// u16len is the length of s in UTF-16 code units.
func u16len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
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
	if trailing && len(lines) > 0 {
		out = append(out, '\n')
	}
	return out
}

func trailingNewline(b []byte) bool { return len(b) > 0 && b[len(b)-1] == '\n' }

// RetainsChanges reports whether head still carries every line edit that
// took base to candidate, untouched: no later edit (candidate -> head)
// overlaps a span candidate inserted or rewrote, inserts inside such a span,
// or inserts where candidate deleted lines, and a final newline candidate
// changed is still the way candidate left it. It is deliberately strict —
// false means "cannot prove it is retained", never "it was reverted".
func RetainsChanges(base, candidate, head []byte) bool {
	if !IsText(base) || !IsText(candidate) || !IsText(head) {
		return false
	}
	lc := splitLines(candidate)
	ours := diffRegions(splitLines(base), lc)
	later := diffRegions(lc, splitLines(head))
	for _, h1 := range ours {
		for _, h2 := range later {
			if h1.toFrom < h1.toTo {
				if h1.from < h1.to {
					// candidate REPLACED base lines: a later edit even abutting
					// the span may be the old lines going back in beside it
					// (a\nb\nc -> a\nX\nc -> a\nX\nb\nc), so it must keep clear.
					if !(h2.to < h1.toFrom || h2.from > h1.toTo) {
						return false
					}
					continue
				}
				// A pure insertion: nothing may touch it or insert strictly
				// inside it; appending right after it is someone else's line.
				if !(h2.to <= h1.toFrom || h2.from >= h1.toTo) {
					return false
				}
				continue
			}
			// A pure deletion at h1.toFrom: nothing may put lines back there.
			if h2.toFrom < h2.toTo && h2.from <= h1.toFrom && h1.toFrom <= h2.to {
				return false
			}
		}
	}
	if trailingNewline(candidate) != trailingNewline(base) && trailingNewline(head) != trailingNewline(candidate) {
		return false
	}
	return true
}
