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
