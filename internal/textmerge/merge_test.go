package textmerge

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// One side inserts before line k while the other replaces from line k. The
// regions do not overlap, so this merges, and it must merge the same way
// whichever side is passed first.
func TestInsertionMeetingADeletionMergesTheSameBothWays(t *testing.T) {
	base := []byte("a\nb\nc\n")
	inserted := []byte("a\nnew\nb\nc\n")
	deleted := []byte("a\nc\n")
	ab, okAB := Merge(base, inserted, deleted)
	ba, okBA := Merge(base, deleted, inserted)
	if !okAB || !okBA {
		t.Fatalf("ok = %v / %v, want both true", okAB, okBA)
	}
	if string(ab) != "a\nnew\nc\n" || string(ba) != string(ab) {
		t.Fatalf("merge(ins, del) = %q, merge(del, ins) = %q, want both %q", ab, ba, "a\nnew\nc\n")
	}
}

// Merge is commutative over every small edit pair: devices that observe one
// concurrency independently must push one blob.
func TestMergeIsCommutative(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	edit := func(base []string) []string {
		out := append([]string(nil), base...)
		i := r.Intn(len(out) + 1)
		switch r.Intn(3) {
		case 0: // insert
			out = append(out[:i], append([]string{fmt.Sprintf("x%d", r.Intn(100))}, out[i:]...)...)
		case 1: // delete
			if i < len(out) {
				out = append(out[:i], out[i+1:]...)
			}
		default: // replace
			if i < len(out) {
				out[i] = fmt.Sprintf("y%d", r.Intn(100))
			}
		}
		return out
	}
	join := func(l []string) []byte {
		if len(l) == 0 {
			return nil
		}
		return []byte(strings.Join(l, "\n") + "\n")
	}
	for n := 0; n < 5000; n++ {
		base := []string{"a", "b", "c", "d", "e"}[:1+r.Intn(5)]
		a, b := edit(base), edit(base)
		ab, okAB := Merge(join(base), join(a), join(b))
		ba, okBA := Merge(join(base), join(b), join(a))
		if okAB != okBA || string(ab) != string(ba) {
			t.Fatalf("base %q a %q b %q: merge(a,b) = %q %v, merge(b,a) = %q %v", base, a, b, ab, okAB, ba, okBA)
		}
	}
}

// The final newline and a blank line are edits like any other.
func TestMergeKeepsNewlineEdits(t *testing.T) {
	for _, c := range []struct{ base, a, b, want string }{
		{"a\n", "a", "a\n", "a"},
		{"a\n", "a\n", "a", "a"},
		{"", "\n", "", "\n"},
		{"a", "a\n", "a", "a\n"},
	} {
		for _, swap := range []bool{false, true} {
			x, y := c.a, c.b
			if swap {
				x, y = y, x
			}
			got, ok := Merge([]byte(c.base), []byte(x), []byte(y))
			if !ok || string(got) != c.want {
				t.Errorf("Merge(%q, %q, %q) = %q, %v; want %q", c.base, x, y, got, ok, c.want)
			}
		}
	}
}

// Two sides that each merged the same third insertion hold its line in
// different places; keeping both insertions would repeat it, so the merge
// declines and the conflict path keeps both versions whole.
func TestSamePointInsertionsSharingALineDecline(t *testing.T) {
	base := "root\n"
	ab, ok := Merge([]byte(base), []byte("root\nA\n"), []byte("root\nB\n"))
	if !ok || string(ab) != "root\nA\nB\n" {
		t.Fatalf("A and B: %q %v", ab, ok)
	}
	bc, ok := Merge([]byte(base), []byte("root\nB\n"), []byte("root\nC\n"))
	if !ok || string(bc) != "root\nB\nC\n" {
		t.Fatalf("B and C: %q %v", bc, ok)
	}
	for _, pair := range [][2][]byte{{ab, bc}, {bc, ab}} {
		if got, ok := Merge([]byte(base), pair[0], pair[1]); ok {
			t.Fatalf("merged two results sharing B into %q", got)
		}
	}
	// A blank line between appended paragraphs is not shared content.
	got, ok := Merge([]byte(base), []byte("root\n\nA\n"), []byte("root\n\nB\n"))
	if !ok || string(got) != "root\n\nA\nB\n" {
		t.Fatalf("blank-led appends: %q %v", got, ok)
	}
	got, ok = Merge([]byte(base), []byte("root\nA\n\n"), []byte("root\nB\n\n"))
	if !ok || string(got) != "root\nA\nB\n\n" {
		t.Fatalf("blank-ended appends: %q %v", got, ok)
	}
}
