package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func op(lamport int64, dev string, seq int64, kind, path, blob string) Op {
	return Op{
		Seq: seq, Lamport: lamport, Time: time.Unix(1000+lamport, 0).UTC(),
		Device: dev, Kind: kind, Path: path, Blob: blob,
	}
}

func TestReplayLastWriterWins(t *testing.T) {
	ops := []Op{
		op(3, "b", 1, KindPut, "a.txt", "v2"),
		op(1, "a", 1, KindPut, "a.txt", "v1"),
		op(2, "a", 2, KindPut, "b.txt", "x"),
	}
	state := Replay(ops)
	if state["a.txt"].Blob != "v2" {
		t.Fatalf("want v2, got %q", state["a.txt"].Blob)
	}
	if state["b.txt"].Blob != "x" {
		t.Fatalf("want x, got %q", state["b.txt"].Blob)
	}
}

func TestReplayDelete(t *testing.T) {
	ops := []Op{
		op(1, "a", 1, KindPut, "a.txt", "v1"),
		op(2, "b", 1, KindDelete, "a.txt", ""),
	}
	if state := Replay(ops); len(state) != 0 {
		t.Fatalf("expected empty state, got %v", state)
	}
	// delete then put resurrects
	ops = append(ops, op(3, "a", 2, KindPut, "a.txt", "v3"))
	if state := Replay(ops); state["a.txt"].Blob != "v3" {
		t.Fatalf("expected v3 after resurrection")
	}
}

func TestOrderTieBreak(t *testing.T) {
	// same lamport + time: device id breaks the tie deterministically
	a := op(5, "aaa", 1, KindPut, "f", "from-a")
	b := op(5, "bbb", 1, KindPut, "f", "from-b")
	a.Time = b.Time
	if state := Replay([]Op{a, b}); state["f"].Blob != "from-b" {
		t.Fatalf("want from-b (higher device id wins tie), got %q", state["f"].Blob)
	}
	if state := Replay([]Op{b, a}); state["f"].Blob != "from-b" {
		t.Fatalf("order of input must not matter")
	}
}

func TestAppendRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dev.jsonl")
	ops := []Op{
		op(1, "a", 1, KindPut, "x.txt", "blob1"),
		op(2, "a", 2, KindDelete, "x.txt", ""),
	}
	if err := Append(p, ops[:1]); err != nil {
		t.Fatal(err)
	}
	if err := Append(p, ops[1:]); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Blob != "blob1" || got[1].Kind != KindDelete {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestReadMissingFile(t *testing.T) {
	got, err := ReadFile(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil || got != nil {
		t.Fatalf("missing journal should be empty, got %v %v", got, err)
	}
}

func TestParseSkipsBlankLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "j.jsonl")
	os.WriteFile(p, []byte("\n{\"seq\":1,\"kind\":\"put\",\"path\":\"a\"}\n\n"), 0o644)
	got, err := ReadFile(p)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v %v", got, err)
	}
}

// TestMtimeIsAdditive pins the wire shape: an op carrying Mtime round-trips,
// and an op without one emits no "mtime" key at all — so a journal written by
// this code still parses in the old shape. (omitempty would not do this: it
// does not omit a zero struct.)
func TestMtimeIsAdditive(t *testing.T) {
	mt := time.Unix(1700000000, 0).UTC()
	with := op(1, "a", 1, KindPut, "x.txt", "blob1")
	with.Mtime = mt
	without := op(2, "a", 2, KindDelete, "x.txt", "")

	data, err := Marshal([]Op{with, without})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if !strings.Contains(lines[0], `"mtime"`) {
		t.Fatalf("put op lost its mtime: %s", lines[0])
	}
	if strings.Contains(lines[1], "mtime") {
		t.Fatalf("op without mtime should emit no mtime key: %s", lines[1])
	}

	got, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Mtime.Equal(mt) {
		t.Fatalf("Mtime = %v, want %v", got[0].Mtime, mt)
	}
	if !got[1].Mtime.IsZero() {
		t.Fatalf("Mtime should be zero, got %v", got[1].Mtime)
	}
}

// TestLastOpsAgreesWithReplay pins LastOps to Replay: every path Replay keeps
// must resolve to an op with the same blob, and a deleted path — which Replay
// drops — must resolve to its delete op.
func TestLastOpsAgreesWithReplay(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	ops := []Op{
		{Seq: 1, Lamport: 1, Time: base, Device: "a", Kind: KindPut, Path: "keep.md", Blob: "aaa"},
		{Seq: 2, Lamport: 5, Time: base.Add(time.Second), Device: "a", Kind: KindPut, Path: "keep.md", Blob: "bbb", UserName: "Dana Kim"},
		{Seq: 1, Lamport: 2, Time: base, Device: "b", Kind: KindPut, Path: "gone.md", Blob: "ccc"},
		{Seq: 2, Lamport: 6, Time: base.Add(2 * time.Second), Device: "b", Kind: KindDelete, Path: "gone.md", UserName: "Sam Ito"},
		{Seq: 3, Lamport: 3, Time: base, Device: "b", Kind: KindPut, Path: "other.md", Blob: "ddd"},
	}
	// Shuffled input must not change the answer.
	shuffled := []Op{ops[3], ops[0], ops[4], ops[2], ops[1]}

	state := Replay(shuffled)
	last := LastOps(shuffled)

	for path, fs := range state {
		op, ok := last[path]
		if !ok {
			t.Fatalf("LastOps missing %q that Replay kept", path)
		}
		if op.Kind != KindPut || op.Blob != fs.Blob {
			t.Fatalf("LastOps[%q] = %+v, want the put with blob %q", path, op, fs.Blob)
		}
	}
	if _, ok := state["gone.md"]; ok {
		t.Fatal("Replay kept a deleted path")
	}
	del, ok := last["gone.md"]
	if !ok || del.Kind != KindDelete || del.UserName != "Sam Ito" {
		t.Fatalf("LastOps[gone.md] = %+v, want Sam Ito's delete", del)
	}
	if got := last["keep.md"].UserName; got != "Dana Kim" {
		t.Fatalf("LastOps[keep.md].UserName = %q, want Dana Kim", got)
	}
}
