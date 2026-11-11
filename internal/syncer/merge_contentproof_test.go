package syncer

import (
	"os"
	"strings"
	"testing"
	"time"
)

// conflictCopies lists the conflict copies of rel in a folder's root.
func conflictCopies(t *testing.T, folder, rel string) []string {
	t.Helper()
	entries, err := os.ReadDir(folder)
	if err != nil {
		t.Fatal(err)
	}
	stem := strings.TrimSuffix(rel, ".md")
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stem) && strings.Contains(e.Name(), "conflict") {
			out = append(out, e.Name())
		}
	}
	return out
}

func mergeOps(t *testing.T, s *Session) int {
	t.Helper()
	ops, err := s.Store.DeviceOps(s.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, op := range ops {
		if op.Note == mergeNote {
			n++
		}
	}
	return n
}

// X₁ → A → X₂ (a revert) → then A again on this device, offline, while a
// peer writes B on X₂ and wins. B's history passes through an OLDER A, but B
// does not hold the A that lost: the edit must be merged into the file or
// preserved as a conflict copy — never dropped because a blob recurs in
// history.
func TestSyncerRevertDoesNotDropConcurrentEdit(t *testing.T) {
	const x = "one\ntwo\nthree\n"
	for _, c := range []struct {
		name, a, b, merged string
	}{
		{name: "disjoint edits merge", a: "ONE\ntwo\nthree\n", b: "one\ntwo\nTHREE\n", merged: "ONE\ntwo\nTHREE\n"},
		{name: "overlapping edits keep a conflict copy", a: "one\nTWO-A\nthree\n", b: "one\nTWO-B\nthree\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			be := sharedRemote(t)
			a := newDevice(t, "deva", be)
			b := newDevice(t, "devb", be)

			write(t, a.Folder, "f.md", x) // X₁
			cycle(t, a)
			cycle(t, b)
			time.Sleep(10 * time.Millisecond)
			write(t, a.Folder, "f.md", c.a) // A₁
			cycle(t, a)
			cycle(t, b)
			time.Sleep(10 * time.Millisecond)
			write(t, b.Folder, "f.md", x) // X₂, the revert
			cycle(t, b)
			cycle(t, a)
			if got := read(t, a.Folder, "f.md"); got != x {
				t.Fatalf("a did not receive the revert: %q", got)
			}

			// A₂ on X₂, journaled offline: it stays unpushed.
			a.Backend = nil
			time.Sleep(10 * time.Millisecond)
			write(t, a.Folder, "f.md", c.a)
			cycle(t, a)

			// B on X₂ wins: b's clock runs ahead with an unrelated edit first.
			write(t, b.Folder, "other.md", "bump")
			cycle(t, b)
			time.Sleep(10 * time.Millisecond)
			write(t, b.Folder, "f.md", c.b)
			cycle(t, b)

			a.Backend = be
			cycle(t, a)
			cycle(t, b)
			cycle(t, a)

			gotA, gotB := read(t, a.Folder, "f.md"), read(t, b.Folder, "f.md")
			if gotA != gotB {
				t.Fatalf("devices diverged: a=%q b=%q", gotA, gotB)
			}
			if c.merged != "" {
				if gotA != c.merged {
					t.Fatalf("f.md = %q, want the merge %q", gotA, c.merged)
				}
				if cc := conflictCopies(t, a.Folder, "f.md"); len(cc) != 0 {
					t.Fatalf("clean merge left conflict copies: %v", cc)
				}
				return
			}
			if gotA != c.b {
				t.Fatalf("f.md = %q, want the winner %q", gotA, c.b)
			}
			for _, s := range []*Session{a, b} {
				cc := conflictCopies(t, s.Folder, "f.md")
				if len(cc) != 1 {
					t.Fatalf("%s: conflict copies = %v, want one holding A", s.Device.Name, cc)
				}
				if got := read(t, s.Folder, cc[0]); got != c.a {
					t.Fatalf("%s: conflict copy = %q, want A's edit %q", s.Device.Name, got, c.a)
				}
			}
		})
	}
}

// When the version that won already holds the local edit — Merge(X, mine, M)
// is byte-for-byte M — the cycle fast-forwards: no merge op and no conflict
// copy duplicate what the winner already says.
func TestSyncerFastForwardsWhenWinnerHoldsLocalEdit(t *testing.T) {
	const (
		x     = "one\ntwo\nthree\n"
		local = "one\ntwo\nTHREE\n"
		m     = "ONE\ntwo\nTHREE\n"
	)
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	h := newDevice(t, "hub", be)

	write(t, a.Folder, "f.md", x)
	cycle(t, a)
	cycle(t, h)

	a.Backend = nil
	time.Sleep(10 * time.Millisecond)
	write(t, a.Folder, "f.md", local)
	cycle(t, a)

	write(t, h.Folder, "other.md", "bump")
	cycle(t, h)
	time.Sleep(10 * time.Millisecond)
	write(t, h.Folder, "f.md", m)
	cycle(t, h)

	a.Backend = be
	res := cycle(t, a)
	if res.Merged != 0 || res.Conflicts != 0 {
		t.Fatalf("fast-forward wrote merged=%d conflicts=%d, want 0/0", res.Merged, res.Conflicts)
	}
	if got := read(t, a.Folder, "f.md"); got != m {
		t.Fatalf("f.md = %q, want the winner %q", got, m)
	}
	if n := mergeOps(t, a); n != 0 {
		t.Fatalf("a journaled %d merge ops, want 0", n)
	}
	if cc := conflictCopies(t, a.Folder, "f.md"); len(cc) != 0 {
		t.Fatalf("fast-forward left conflict copies: %v", cc)
	}
	cycle(t, h)
	if got := read(t, h.Folder, "f.md"); got != m {
		t.Fatalf("hub f.md = %q, want %q", got, m)
	}
}
