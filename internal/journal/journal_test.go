package journal

import (
	"os"
	"path/filepath"
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
