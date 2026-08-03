package journal

// Round 5 — the target is round 4's own fixes (b616c94) in this package:
// Parse's "skip one bad line", the path_raw byte-exact path carrier, and the
// extension of Less into a claimed total order.
//
// Every test asserts the SECURE behavior, so it goes green the moment the hole
// is closed and stays as a permanent regression test. Helpers are prefixed
// secfx4; no existing file is touched.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

// ---- helpers -------------------------------------------------------------

func secfx4Line(t *testing.T, op Op) string {
	t.Helper()
	b, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func secfx4Paths(ops []Op) []string {
	out := make([]string, 0, len(ops))
	for _, o := range ops {
		out = append(out, o.Kind+":"+o.Path)
	}
	return out
}

// ---------------------------------------------------------------------------
// Parse: "a line that does not decode is skipped" has to mean the line
// produces NO op. `null` and `{}` decode without error into nothing.
// ---------------------------------------------------------------------------

// Round 4 replaced all-or-nothing parsing with skip-the-bad-line, and the
// justification is explicitly about counting: "the ops that did decode are
// still the device's history". Op counts are not cosmetic in this codebase —
// they are the two cursors the sync engine runs on:
//
//	syncer.pull:   newOps = fresh[len(prev):]
//	syncer.commit: seqBase := int64(len(myOps))
//	syncer.push:   st.PushedOps = int64(len(myOps))
//
// so a line that yields a phantom op is a line that moves another device's
// cursor. `null` is the cheapest one: encoding/json calls Op.UnmarshalJSON
// with the literal `null`, the inner json.Unmarshal treats it as a no-op and
// returns nil, and Parse appends a zero-valued Op. `{}` does the same through
// the ordinary path. Neither is an operation anybody journaled.
func TestSec_Parse_ALineThatIsNotAnOpProducesNoOp(t *testing.T) {
	real := secfx4Line(t, Op{Seq: 1, Lamport: 1, Device: "d", Kind: KindPut, Path: "a.md", Blob: "b"})
	for _, junk := range []string{"null", "{}", `{"nothing":1}`} {
		ops, err := Parse([]byte(junk + "\n" + real))
		if err != nil {
			t.Fatalf("Parse(%q): %v", junk, err)
		}
		if len(ops) != 1 {
			t.Errorf("Parse of one real op preceded by %q returned %d ops, want 1: %v\n"+
				"a line that names no path and no kind is not an operation, but Parse counts it — "+
				"and op counts are the cursors syncer.pull (fresh[len(prev):]) and syncer.commit "+
				"(seqBase = len(myOps)) run on, so a peer pads them at will",
				junk, len(ops), secfx4Paths(ops))
		}
	}
}

// The framing question round 4's change opens: a peer writes the whole file,
// so can one line it chooses make Parse lose a GOOD op? bytes.Split on "\n"
// should make every line independent — assert it, because the obvious
// "improvement" back to a bufio.Scanner reintroduces a length ceiling that a
// long line uses to truncate everything after it.
func TestSec_Parse_OneBadLineNeverSwallowsTheOpsAroundIt(t *testing.T) {
	good1 := secfx4Line(t, Op{Seq: 1, Lamport: 1, Device: "d", Kind: KindPut, Path: "keep1.md", Blob: "b1"})
	good2 := secfx4Line(t, Op{Seq: 2, Lamport: 2, Device: "d", Kind: KindDelete, Path: "keep2.md"})

	huge := `{"seq":3,"kind":"put","path":"` + string(make([]byte, 20<<20)) + `"` // unterminated, 20MB
	for name, junk := range map[string]string{
		"unterminated json": `{"seq":9,"kind":"put","path":"x`,
		"trailing garbage":  `{"seq":9,"kind":"put","path":"x"} trailing`,
		"escaped newline":   `{"seq":9,"kind":"put","path":"a\nb"} `,
		"20MB line":         huge,
		"nul bytes":         "\x00\x00\x00",
		"deep nesting":      `{"seq":` + fmt.Sprint(rand.Int63()) + `,"x":[[[[[[[`,
	} {
		ops, err := Parse([]byte(good1 + junk + "\n" + good2))
		if err != nil {
			t.Fatalf("%s: Parse: %v", name, err)
		}
		var kept []string
		for _, o := range ops {
			kept = append(kept, o.Path)
		}
		if len(ops) < 2 || ops[0].Path != "keep1.md" || ops[len(ops)-1].Path != "keep2.md" {
			t.Errorf("%s: a crafted line swallowed a neighbouring op: kept %v", name, kept)
		}
	}
}

// ---------------------------------------------------------------------------
// path_raw: one op, two paths.
// ---------------------------------------------------------------------------

// Round 4 added path_raw so a path that is not valid UTF-8 survives JSON
// byte-exactly. UnmarshalJSON applies it UNCONDITIONALLY, though — it never
// checks that the base64 decodes to the same bytes the `path` field carries.
// A peer therefore writes one op that names two different files:
//
//	{"kind":"put","path":"notes.md","path_raw":"<base64 of anything>"}
//
// Every reader with round 4's journal package materializes the path_raw value;
// every reader without it — an older bdrive on a teammate's laptop, and any
// other consumer of the JSONL (the archive `bdrive export` writes, a hub that
// has not been redeployed) — materializes "notes.md". Two devices replaying
// the same journal converge to different states, which is the one thing
// CLAUDE.md's invariants say must never happen, and the split is chosen by
// whoever wrote the line.
//
// The fix is cheap: apply path_raw only when the `path` field is the lossy
// encoding of those same bytes.
func TestSec_Op_PathRawCannotNameADifferentPathThanPath(t *testing.T) {
	line := `{"seq":1,"lamport":1,"device":"peer","kind":"put","path":"notes.md","path_raw":"Li4vLi4vLmJkcml2ZS9jb25maWcuanNvbg==","blob":"b"}`

	ops, err := Parse([]byte(line + "\n"))
	if err != nil || len(ops) != 1 {
		t.Fatalf("Parse: %v (%d ops)", err, len(ops))
	}

	var old struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(line), &old); err != nil {
		t.Fatal(err)
	}

	if ops[len(ops)-1].Path != old.Path {
		t.Errorf("one journal line names two different paths:\n"+
			"  round-4 reader materializes %q\n"+
			"  every other reader of the same bytes materializes %q\n"+
			"path_raw is applied without checking it is the byte-exact source of the `path` field, "+
			"so a peer picks which devices in a mixed fleet see which file",
			ops[len(ops)-1].Path, old.Path)
	}
}

// The legitimate case the field exists for must keep working: a genuinely
// non-UTF-8 path round-trips byte-exactly.
func TestSec_Op_InvalidUTF8PathStillRoundTrips(t *testing.T) {
	want := "caf\xff.md"
	line := secfx4Line(t, Op{Seq: 1, Device: "d", Kind: KindPut, Path: want, Blob: "b"})
	ops, err := Parse([]byte(line))
	if err != nil || len(ops) != 1 {
		t.Fatalf("Parse: %v (%d ops)", err, len(ops))
	}
	if ops[0].Path != want {
		t.Errorf("invalid-UTF-8 path did not survive the round trip: %q, want %q", ops[0].Path, want)
	}
}

// ---------------------------------------------------------------------------
// Less: is it actually a total order, and is Replay permutation-independent?
// ---------------------------------------------------------------------------

func secfx4Corpus() []Op {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var ops []Op
	for _, lam := range []int64{0, 1, 1} {
		for _, tm := range []time.Time{t0, t0.Add(time.Nanosecond), t0.In(time.FixedZone("x", 3600))} {
			for _, dev := range []string{"a", "b"} {
				for _, seq := range []int64{1, 2} {
					for _, kind := range []string{KindPut, KindDelete} {
						for _, p := range []string{"x.md", "y.md"} {
							for _, blob := range []string{"", "b1"} {
								ops = append(ops, Op{
									Lamport: lam, Time: tm, Device: dev, Seq: seq,
									Kind: kind, Path: p, Blob: blob, Size: int64(len(blob)),
									// fields Less does NOT compare
									User: "u" + dev, Author: dev, Note: p, Mtime: tm,
								})
							}
						}
					}
				}
			}
		}
	}
	return ops
}

// Antisymmetry and transitivity over a corpus built from every tie the
// comparator can produce. A comparator that is not a strict weak ordering
// makes sort.SliceStable's output depend on the input permutation, which is
// exactly what round 4 extended Less to prevent.
func TestSec_Less_IsAStrictWeakOrdering(t *testing.T) {
	ops := secfx4Corpus()
	for i, a := range ops {
		if Less(a, a) {
			t.Fatalf("Less is not irreflexive at %d", i)
		}
		for j, b := range ops {
			if Less(a, b) && Less(b, a) {
				t.Fatalf("Less(%d,%d) and Less(%d,%d) both true", i, j, j, i)
			}
			for _, c := range ops {
				if Less(a, b) && Less(b, c) && !Less(a, c) {
					t.Fatalf("Less is not transitive:\n a=%+v\n b=%+v\n c=%+v", a, b, c)
				}
				// equivalence (neither less) must also be transitive
				eqAB := !Less(a, b) && !Less(b, a)
				eqBC := !Less(b, c) && !Less(c, b)
				eqAC := !Less(a, c) && !Less(c, a)
				if eqAB && eqBC && !eqAC {
					t.Fatalf("incomparability is not transitive:\n a=%+v\n b=%+v\n c=%+v", a, b, c)
				}
			}
		}
	}
}

// Replay under every permutation the caller might collect ops in. One journal
// per device is read in whatever order List returned, so the permutation is
// not the caller's choice — and two devices disagreeing here is the
// convergence invariant breaking.
func TestSec_Replay_IsIndependentOfInputOrder(t *testing.T) {
	ops := secfx4Corpus()
	want := Replay(ops)
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 200; i++ {
		shuffled := append([]Op(nil), ops...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if got := Replay(shuffled); !reflect.DeepEqual(got, want) {
			t.Fatalf("Replay depends on input order (permutation %d):\n got  %v\n want %v", i, got, want)
		}
	}
}
