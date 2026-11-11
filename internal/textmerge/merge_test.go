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

// Two insertions at one point of a file both survive, in an order picked by
// content; over an EMPTY ancestor the point is the whole file, and two
// different bodies there are two documents, never spliced into one.
func TestSamePointInsertions(t *testing.T) {
	for _, c := range []struct {
		name, base, a, b, want string
		ok                     bool
	}{
		{"inside a file: both kept", "a\nc\n", "a\nFROM-A\nc\n", "a\nFROM-B\nc\n", "a\nFROM-A\nFROM-B\nc\n", true},
		{"inside a file: one append repeating the other is two appends", "log\n", "log\nx\n", "log\nx\nx\n", "log\nx\nx\nx\n", true},
		{"empty ancestor: two different bodies", "", "x\n", "y\n", "", false},
		{"empty ancestor: one body holds the other", "", "a\n", "a\nb\n", "a\nb\n", true},
		{"empty ancestor: the same body", "", "x\n", "x\n", "x\n", true},
	} {
		for _, swap := range []bool{false, true} {
			x, y := c.a, c.b
			if swap {
				x, y = y, x
			}
			got, ok := Merge([]byte(c.base), []byte(x), []byte(y))
			if ok != c.ok || (ok && string(got) != c.want) {
				t.Errorf("%s: Merge(%q, %q, %q) = %q, %v; want %q, %v", c.name, c.base, x, y, got, ok, c.want, c.ok)
			}
		}
	}
}

func TestRetainsChanges(t *testing.T) {
	cases := []struct {
		name             string
		base, cand, head string
		want             bool
	}{
		{"T5 append after", "- item 1\n", "- item 1\n- alice\n", "- item 1\n- alice\n- bob\n", true},
		{"edit elsewhere", "a\nb\nc\nd\n", "a\nB\nc\nd\n", "a\nB\nc\nD\n", true},
		{"head removed the line", "a\nc\n", "a\nA\nc\n", "a\nc\nB\n", false},
		{"head rewrote the line", "a\nb\nc\n", "a\nA\nc\n", "a\nZ\nc\n", false},
		{"head inserted inside span", "a\nd\n", "a\nb\nc\nd\n", "a\nb\nX\nc\nd\n", false},
		{"deletion kept", "a\nb\nc\n", "a\nc\n", "a\nc\nd\n", true},
		{"deletion undone", "a\nb\nc\n", "a\nc\n", "a\nb\nc\n", false},
		{"newline change undone", "a", "a\n", "a", false},
		{"newline change kept", "a", "a\n", "a\nb\n", true},
		{"binary", "a\x00", "a\x00b", "a\x00b", false},
		{"replaced line restored beside it", "a\nb\nc\n", "a\nX\nc\n", "a\nX\nb\nc\n", false},
		{"replaced line restored before it", "a\nb\nc\n", "a\nX\nc\n", "a\nb\nX\nc\n", false},
	}
	for _, c := range cases {
		if got := RetainsChanges([]byte(c.base), []byte(c.cand), []byte(c.head)); got != c.want {
			t.Errorf("%s: RetainsChanges = %v, want %v", c.name, got, c.want)
		}
	}
}
