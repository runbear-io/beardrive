package webapp

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf16"
)

// applyHunks is the text hunks turn `from` into.
func applyHunks(from string, hunks []textHunk) string {
	u := utf16.Encode([]rune(from))
	var sb strings.Builder
	last := 0
	for _, h := range hunks {
		sb.WriteString(string(utf16.Decode(u[last:h.at])))
		sb.WriteString(h.ins)
		last = h.at + h.del
	}
	sb.WriteString(string(utf16.Decode(u[last:])))
	return sb.String()
}

func TestTextHunksRebuildTheTarget(t *testing.T) {
	alphabet := []string{"a", "b", "\n", "日", "✅", "😀", " "}
	gen := func(r *rand.Rand) string {
		var sb strings.Builder
		for i := r.Intn(40); i > 0; i-- {
			sb.WriteString(alphabet[r.Intn(len(alphabet))])
		}
		return sb.String()
	}
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 5000; i++ {
		from, to := gen(r), gen(r)
		if got := applyHunks(from, textHunks(from, to)); got != to {
			t.Fatalf("textHunks(%q, %q) rebuilt %q", from, to, got)
		}
	}
}

// Two separate line changes are two edits, so the untouched lines between
// them are never deleted and put back.
func TestTextHunksLeaveUntouchedLinesAlone(t *testing.T) {
	from := "title\nkeep one\nold\nkeep two\nend\n"
	to := "TITLE\nkeep one\nold\nkeep two\nend\nadded\n"
	hunks := textHunks(from, to)
	if len(hunks) != 2 {
		t.Fatalf("got %d hunks %+v, want 2", len(hunks), hunks)
	}
	if applyHunks(from, hunks) != to {
		t.Fatal("hunks do not rebuild the target")
	}
	for _, h := range hunks {
		if h.del > len("title") {
			t.Fatalf("hunk %+v deletes more than the line that changed", h)
		}
	}
}

func TestTextHunksFallBackPastTheLimit(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 3*maxDiffLines; i++ {
		a.WriteString("a\n")
		b.WriteString("b\n")
	}
	if got := applyHunks(a.String(), textHunks(a.String(), b.String())); got != b.String() {
		t.Fatal("past the diff limit the hunks no longer rebuild the target")
	}
}
