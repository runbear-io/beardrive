package journal

import (
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Security tests for the op log. Two trust boundaries meet here: the bytes of
// a peer's journal (pulled from the hub and parsed verbatim) and the bytes of
// this device's own journal (appended non-atomically by every cycle).

func secpkgOp(seq int64, path string) Op {
	return Op{
		Seq: seq, Lamport: seq, Time: time.Unix(1700000000, int64(seq)).UTC(),
		Device: "dev-1", Kind: KindPut, Path: path,
		Blob: strings.Repeat("a", 64), Size: 3,
	}
}

// TestSec_Journal_TornTailDoesNotVoidTheWholeJournal: Append is a plain
// O_APPEND Write, so it is not atomic — a crash, a full disk or a kill mid
// write leaves a partial final line. Parse is all-or-nothing, so that single
// torn byte range makes ReadFile reject EVERY op the device ever committed.
// The volume then cannot replay, cannot sync, and there is no recovery path
// in the CLI.
func TestSec_Journal_TornTailDoesNotVoidTheWholeJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev-1.jsonl")
	ops := []Op{secpkgOp(1, "a.md"), secpkgOp(2, "b.md"), secpkgOp(3, "c.md")}
	if err := Append(path, ops); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the interrupted write: keep everything up to and including the
	// second op's newline, plus half of the third line.
	full := strings.SplitAfter(string(data), "\n")
	torn := full[0] + full[1] + full[2][:len(full[2])/2]
	if err := os.WriteFile(path, []byte(torn), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("a torn final line voided the whole journal: ReadFile = %v (the 2 complete ops before it are now unreadable)", err)
	}
	if len(got) < 2 {
		t.Fatalf("recovered %d ops from a torn journal, want the 2 complete ones", len(got))
	}
}

// TestSec_Journal_OneUnreadableLineCannotVoidTheOpsBeforeIt: same
// all-or-nothing weakness, reached deliberately. Parse's scanner caps a line
// at 16 MiB and its json.Unmarshal error aborts the whole file, so one line
// planted in a journal (or a legitimately huge path) discards every op that
// parsed fine before it.
func TestSec_Journal_OneUnreadableLineCannotVoidTheOpsBeforeIt(t *testing.T) {
	good, err := Marshal([]Op{secpkgOp(1, "a.md"), secpkgOp(2, "b.md")})
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"oversized-line":  `{"path":"` + strings.Repeat("A", 17<<20) + `"}`,
		"not-json":        `{"seq":1,`,
		"wrong-type":      `{"seq":"one","kind":"put","path":"x"}`,
		"deeply-nested":   strings.Repeat("[", 20000) + strings.Repeat("]", 20000),
		"number-overflow": `{"lamport":1e400,"kind":"put","path":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			ops, err := Parse(append(append([]byte(nil), good...), []byte(bad+"\n")...))
			if err != nil {
				t.Fatalf("one %s line voided the journal: %v (the 2 valid ops before it are gone)", name, err)
			}
			if len(ops) < 2 {
				t.Fatalf("kept %d ops, want at least the 2 valid ones before the bad line", len(ops))
			}
		})
	}
}

// TestSec_Journal_PathSurvivesTheWireFormatByteExact: Op.Path comes from the
// filesystem, where any byte but NUL and '/' is a legal filename. encoding/json
// silently rewrites invalid UTF-8 to U+FFFD, so Marshal→Parse is not the
// identity: the path a device journals is not the path its peers replay, and —
// worse — two distinct files collapse onto one path, so one silently
// overwrites the other on every other device.
func TestSec_Journal_PathSurvivesTheWireFormatByteExact(t *testing.T) {
	a := "notes/caf\xe9.md" // latin-1 é — a legal unix filename
	b := "notes/caf\xff.md" // a different legal unix filename
	round := func(p string) string {
		data, err := Marshal([]Op{secpkgOp(1, p)})
		if err != nil {
			t.Fatal(err)
		}
		ops, err := Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		return ops[0].Path
	}
	if got := round(a); got != a {
		t.Errorf("path %q round-tripped as %q: peers replay a different filename", a, got)
	}
	if round(a) == round(b) {
		t.Errorf("two distinct paths (%q, %q) both round-tripped to %q: one file silently overwrites the other on every peer", a, b, round(a))
	}
}

// TestSec_Journal_ReplayIsDeterministicUnderInputPermutation asserts the
// invariant CLAUDE.md names — the same ops fold to the same state — as a
// property of the ops, not of the order a caller happened to collect them in.
// Less is not a total order: a peer can write two ops with the same
// (lamport, time, device, seq) and different content, and Sort's stability
// then makes the winner the caller's input sequence. Today every caller
// reaches Replay through Store.AllOps, whose order is incidentally stable
// (sorted device files, byte-identical journals), so this has no cross-device
// divergence reproducer — the guarantee is carried by a caller's accident
// rather than by Less, and any new caller that collects ops from a map, a
// concurrent fetch, or a differently-ordered backend listing loses it.
func TestSec_Journal_ReplayIsDeterministicUnderInputPermutation(t *testing.T) {
	base := []Op{
		secpkgOp(1, "a.md"), secpkgOp(2, "b.md"), secpkgOp(3, "a.md"),
		{Seq: 4, Lamport: 4, Time: time.Unix(1700000000, 4).UTC(), Device: "dev-2", Kind: KindDelete, Path: "b.md"},
		// A hostile peer reusing a tuple: Less says neither is smaller.
		{Seq: 5, Lamport: 5, Time: time.Unix(1700000000, 5).UTC(), Device: "dev-2", Kind: KindPut, Path: "c.md", Blob: strings.Repeat("b", 64)},
		{Seq: 5, Lamport: 5, Time: time.Unix(1700000000, 5).UTC(), Device: "dev-2", Kind: KindPut, Path: "c.md", Blob: strings.Repeat("c", 64)},
	}
	want := Replay(base)
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 200; i++ {
		shuffled := append([]Op(nil), base...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if got := Replay(shuffled); !reflect.DeepEqual(got, want) {
			t.Fatalf("Replay is order-dependent: permutation %d folded to %v, want %v", i, got, want)
		}
	}
}
