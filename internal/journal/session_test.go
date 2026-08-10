package journal

import (
	"strings"
	"testing"
)

// Op.Session holds the same standing as Mtime: additive on the wire, and
// invisible to the ordering. A journal written by this code still parses in
// the old shape, and an op written before the field existed reads back as "".
func TestSessionIsAdditive(t *testing.T) {
	with := op(1, "a", 1, KindPut, "x.txt", "blob1")
	with.Session = "8f21e4"
	without := op(2, "a", 2, KindPut, "y.txt", "blob2")

	data, err := Marshal([]Op{with, without})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if !strings.Contains(lines[0], `"session":"8f21e4"`) {
		t.Fatalf("op lost its session: %s", lines[0])
	}
	if strings.Contains(lines[1], "session") {
		t.Fatalf("op without a session should emit no session key: %s", lines[1])
	}

	got, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Session != "8f21e4" {
		t.Fatalf("Session = %q, want 8f21e4", got[0].Session)
	}
	if got[1].Session != "" {
		t.Fatalf("Session should be empty, got %q", got[1].Session)
	}
	// A line written by an older device carries no session key at all.
	legacy, err := Parse([]byte(`{"seq":1,"lamport":1,"device":"a","kind":"put","path":"z.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if legacy[0].Session != "" {
		t.Fatalf("legacy op invented a session: %q", legacy[0].Session)
	}
}

// Replay determinism is the invariant this field must not touch: two ops
// differing ONLY in Session compare equal under Less in both directions, so
// no peer's ordering can depend on it.
func TestSessionDoesNotOrder(t *testing.T) {
	a := op(1, "dev", 1, KindPut, "x.txt", "blob1")
	b := a
	b.Session = "8f21e4"
	if Less(a, b) || Less(b, a) {
		t.Fatalf("Session must not be an input to Less: Less(a,b)=%v Less(b,a)=%v", Less(a, b), Less(b, a))
	}
}
