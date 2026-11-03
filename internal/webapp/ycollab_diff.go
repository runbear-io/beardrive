package webapp

import "unicode/utf16"

// textHunk replaces del UTF-16 units at `at` with ins. Offsets are in the
// text the hunks were computed against.
type textHunk struct {
	at, del int
	ins     string
}

// maxDiffLines bounds the line diff. Past it the texts are treated as one
// replaced region, which is still correct, only coarser.
const maxDiffLines = 1000

/*
textHunks is every place `to` differs from `from`, as separate replacements.

	One replacement over the whole changed span would be the same text, but not
	the same edit. Folded into a document that people kept typing in, a span
	covering untouched lines deletes and re-inserts them, and anything a
	co-editor changed inside them comes back alongside the new text. So the
	diff is by line, and each changed run of lines is then narrowed to the
	characters that actually differ.
*/
func textHunks(from, to string) []textHunk {
	a, b := []rune(from), []rune(to)
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	n := 0
	for n < len(a)-p && n < len(b)-p && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	if p == len(a) && p == len(b) {
		return nil
	}
	am, bm := a[p:len(a)-n], b[p:len(b)-n]
	la, lb := splitLines(am), splitLines(bm)
	type span struct{ a0, a1, b0, b1 int } // rune offsets into am, bm
	var spans []span
	if regions, ok := lineDiff(la, lb, maxDiffLines); ok {
		ao, bo := lineOffsets(la), lineOffsets(lb)
		for _, r := range regions {
			spans = append(spans, span{ao[r[0]], ao[r[1]], bo[r[2]], bo[r[3]]})
		}
	} else {
		spans = []span{{0, len(am), 0, len(bm)}}
	}

	hunks := make([]textHunk, 0, len(spans))
	at, cursor := u16(a[:p]), 0 // UTF-16 offset of am[cursor]
	for _, s := range spans {
		x, y := am[s.a0:s.a1], bm[s.b0:s.b1]
		q := 0
		for q < len(x) && q < len(y) && x[q] == y[q] {
			q++
		}
		r := 0
		for r < len(x)-q && r < len(y)-q && x[len(x)-1-r] == y[len(y)-1-r] {
			r++
		}
		at += u16(am[cursor : s.a0+q])
		del := u16(x[q : len(x)-r])
		hunks = append(hunks, textHunk{at: at, del: del, ins: string(y[q : len(y)-r])})
		at += del
		cursor = s.a0 + len(x) - r
	}
	return hunks
}

func u16(rs []rune) int { return len(utf16.Encode(rs)) }

// splitLines cuts runes into lines that keep their "\n", so the lines join
// back into exactly the input.
func splitLines(rs []rune) []string {
	var out []string
	start := 0
	for i, r := range rs {
		if r == '\n' {
			out = append(out, string(rs[start:i+1]))
			start = i + 1
		}
	}
	if start < len(rs) {
		out = append(out, string(rs[start:]))
	}
	return out
}

// lineOffsets is the rune offset at which each line starts, plus the total.
func lineOffsets(lines []string) []int {
	out := make([]int, len(lines)+1)
	for i, l := range lines {
		out[i+1] = out[i] + len([]rune(l))
	}
	return out
}

/*
lineDiff is Myers' diff over lines: the regions [a0,a1) of a replaced by
[b0,b1) of b, in order, everything between them equal. False when the texts
differ in more than maxD lines.

	The trace keeps each round's frontier at its own width (2d+1), so memory is
	O(D²) in the number of differing lines rather than D times the file.
*/
func lineDiff(a, b []string, maxD int) ([][4]int, bool) {
	n, m := len(a), len(b)
	off := maxD + 1
	v := make([]int, 2*off+1)
	var trace [][]int
	found := false
	for d := 0; d <= maxD && !found; d++ {
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
				found = true
				break
			}
		}
		trace = append(trace, append([]int(nil), v[off-d:off+d+1]...))
	}
	if !found {
		return nil, false
	}

	// Walk back from the end, collecting the lines both sides share.
	var same [][2]int
	x, y := n, m
	for d := len(trace) - 1; d > 0; d-- {
		prev := trace[d-1]
		at := func(k int) int { return prev[k+d-1] }
		k := x - y
		var pk int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := at(pk)
		py := px - pk
		mx, my := px, py
		if pk == k-1 {
			mx++
		} else {
			my++
		}
		for x > mx && y > my {
			x--
			y--
			same = append(same, [2]int{x, y})
		}
		x, y = px, py
	}
	for x > 0 && y > 0 {
		x--
		y--
		same = append(same, [2]int{x, y})
	}

	var out [][4]int
	i, j := 0, 0
	for s := len(same) - 1; s >= 0; s-- {
		si, sj := same[s][0], same[s][1]
		if si > i || sj > j {
			out = append(out, [4]int{i, si, j, sj})
		}
		i, j = si+1, sj+1
	}
	if i < n || j < m {
		out = append(out, [4]int{i, n, j, m})
	}
	return out, true
}
