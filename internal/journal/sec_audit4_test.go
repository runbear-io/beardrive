package journal

// Round 9, row 17 (internal/journal) — replacement tests for guards that
// survived a hand reversion with the WHOLE TestSec suite green. Each one is
// written so that only the guard under test can produce the refusal.
//
// Helpers are prefixed secaud4.

import (
	"strings"
	"testing"
	"time"
)

// secaud4Round marshals one op and parses it back, the way a peer's journal
// travels: this device writes JSONL, the hub stores the bytes verbatim, every
// other device parses them.
func secaud4Round(t *testing.T, p string) string {
	t.Helper()
	data, err := Marshal([]Op{{Seq: 1, Lamport: 1, Device: "d", Kind: KindPut, Path: p, Blob: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	ops, err := Parse(data)
	if err != nil || len(ops) != 1 {
		t.Fatalf("Parse: %v (%d ops)", err, len(ops))
	}
	return ops[0].Path
}

// TestSec_Op_AdjacentInvalidBytesInAPathDoNotCollapseTwoFilesIntoOne.
//
// `lossy` is this package's model of what encoding/json does to a string that
// is not valid UTF-8, and it is the whole basis of round 5's fix: path_raw is
// applied only when it re-encodes to the `path` field the line already carries.
// The model has to be per-BYTE. strings.ToValidUTF8 collapses a RUN of invalid
// bytes into a single U+FFFD while encoding/json emits one U+FFFD per byte, so
// a run-collapsing model makes the re-encode check reject the path_raw of a
// perfectly legitimate filename that happens to have two bad bytes in a row —
// and every existing test uses a SINGLE invalid byte, where the two models
// agree, so the per-byte loop could be replaced by ToValidUTF8 with the whole
// suite green.
//
// The consequence is exactly what round 4 added path_raw to prevent, back
// again for the paths that need it most: the op falls back to the lossy `path`
// field, and two distinct, legal unix filenames arrive at every peer as the
// same path, so one file silently overwrites the other.
func TestSec_Op_AdjacentInvalidBytesInAPathDoNotCollapseTwoFilesIntoOne(t *testing.T) {
	// Control: one invalid byte, the case the existing suite covers.
	if got, want := secaud4Round(t, "notes/caf\xff.md"), "notes/caf\xff.md"; got != want {
		t.Fatalf("control: %q round-tripped as %q", want, got)
	}

	for _, want := range []string{
		"notes/caf\xff\xfe.md",
		"notes/caf\xfe\xff.md",
		"\xc3\x28\xa0\xa1.md", // broken sequences, adjacent bad bytes
		"a\xff\xff\xffb.md",
		"\xff\xff",
	} {
		if got := secaud4Round(t, want); got != want {
			t.Errorf("path %q round-tripped as %q: path_raw was refused for a path that needs "+
				"it, so every peer replays a different filename than the one on disk", want, got)
		}
	}

	a, b := "notes/caf\xff\xfe.md", "notes/caf\xfe\xff.md"
	if secaud4Round(t, a) == secaud4Round(t, b) {
		t.Errorf("two distinct legal filenames (%q, %q) both round-tripped to %q: one file "+
			"silently overwrites the other on every peer", a, b, secaud4Round(t, a))
	}
}

// TestSec_Less_OrdersEveryPairItCanTellApart.
//
// Less's doc comment claims a TOTAL order and lists its comparisons in one
// chain. TestSec_Less_IsAStrictWeakOrdering proves the axioms, which any
// PREFIX of that chain also satisfies, and TestSec_Replay_IsIndependentOfInputOrder
// only notices a dropped field that Replay reads — so the (lamport, time,
// device, seq) part could lose `seq` with the entire suite green.
//
// The property that actually matters is the one the invariant rests on: if two
// ops differ in any field the comparator reads, the comparator must ORDER them
// rather than declare them equivalent. An equivalence class holding two
// different ops is resolved by sort.SliceStable, i.e. by whatever order the
// caller happened to collect the journals in — and a peer chooses the tuple, so
// it chooses which devices see which member first.
func TestSec_Less_OrdersEveryPairItCanTellApart(t *testing.T) {
	base := Op{
		Lamport: 7,
		Time:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Device:  "dev-1",
		Seq:     3,
		Kind:    KindPut,
		Path:    "notes.md",
		Blob:    strings.Repeat("a", 64),
		Size:    12,
		Mode:    0o644,
	}
	// One field changed at a time — each is a comparison Less claims to make.
	variants := map[string]Op{}
	for name, mut := range map[string]func(Op) Op{
		"lamport": func(o Op) Op { o.Lamport++; return o },
		"time":    func(o Op) Op { o.Time = o.Time.Add(time.Nanosecond); return o },
		"device":  func(o Op) Op { o.Device = "dev-2"; return o },
		"seq":     func(o Op) Op { o.Seq++; return o },
		"kind":    func(o Op) Op { o.Kind = KindDelete; return o },
		"path":    func(o Op) Op { o.Path = "other.md"; return o },
		"blob":    func(o Op) Op { o.Blob = strings.Repeat("b", 64); return o },
		"size":    func(o Op) Op { o.Size++; return o },
		"mode":    func(o Op) Op { o.Mode = 0o600; return o },
	} {
		variants[name] = mut(base)
	}
	for name, v := range variants {
		if !Less(base, v) && !Less(v, base) {
			t.Errorf("Less cannot order two ops that differ only in %s — they land in one "+
				"equivalence class, and sort.SliceStable then resolves them by the order the "+
				"caller collected the journals in, which is not a property of the ops",
				name)
		}
		if Less(base, v) && Less(v, base) {
			t.Errorf("Less(%s) is not antisymmetric", name)
		}
	}
	// And the base is still equivalent only to itself.
	if Less(base, base) {
		t.Error("Less is not irreflexive")
	}
}
