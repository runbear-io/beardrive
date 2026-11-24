package webapp

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

func waitForCheckpointWrite(t *testing.T, rs *RemoteSource, be remote.Backend, key string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rs.ckptMu.Lock()
		writing := rs.ckptWriting
		rs.ckptMu.Unlock()
		ok, err := be.Exists(context.Background(), key)
		if !writing && err == nil && ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("checkpoint %s did not finish writing in time", key)
}

func TestQA14_FoldCheckpointColdRestoreImmutableVsMutableAndNewJournal(t *testing.T) {
	ctx := context.Background()

	t.Run("ImmutableColdRestoreZeroRPCsUnchangedAndTailOnlyGrown", func(t *testing.T) {
		be := newTailTestMetaBackend(true)
		db, err := OpenProjectDB(filepath.Join(t.TempDir(), "projects.json"))
		if err != nil {
			t.Fatal(err)
		}
		p, _, err := db.GetOrCreate("qa14-imm", "")
		if err != nil {
			t.Fatal(err)
		}
		raw, blob := seedLargeMetaJournal(t, be, p.ID, "dev1", 10000)
		srv := &Server{Root: be, Projects: db, Upload: UploadConfig{Enabled: true}}
		_, v, err := srv.projectVolume(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		warmRS := v.source.(*RemoteSource)
		if _, err := warmRS.Files(ctx); err != nil {
			t.Fatal(err)
		}
		waitForCheckpointWrite(t, warmRS, be, p.ID+"/"+hubFoldCheckpointKey)

		// Cold RemoteSource on immutable backend: unchanged journal must take ZERO Get/Range RPCs!
		coldRS := &RemoteSource{Backend: remote.Prefixed(be, p.ID)}
		be.resetStats()
		files, err := coldRS.Files(ctx)
		if err != nil || len(files) != 10000 {
			t.Fatalf("cold restore unchanged: len=%d err=%v", len(files), err)
		}
		fg, fb, rg, rb := be.stats()
		if fg != 0 || fb != 0 || rg != 0 || rb != 0 {
			t.Fatalf("immutable cold restore unchanged transferred journal bytes: full=%d(%d) range=%d(%d)", fg, fb, rg, rb)
		}

		// CRC32C mismatch on immutable unchanged journal must fail compatibility check and
		// refuse the 0-RPC shortcut (falling back to a full prefix GET verification).
		badCRCObj := remote.Object{
			Key: "journal/dev1.jsonl", Size: int64(len(raw)), Version: "1", ImmutableVersion: true, CRC32C: 0xdeadbeef,
		}
		badMeta := ckptJournalMeta{
			Size: int64(len(raw)), PrefixSHA256: qaHexSHA(raw), Version: "1", CRC32C: crc32cSum(raw),
		}
		if compatibleCheckpointJournals(map[string]ckptJournalMeta{"journal/dev1.jsonl": badMeta}, []remote.Object{badCRCObj}) {
			t.Fatal("compatibleCheckpointJournals accepted mismatched CRC32C")
		}
		be.resetStats()
		if _, _, ok := coldRS.verifyAndReadTail(ctx, badCRCObj, badMeta, true, true); !ok {
			t.Fatal("expected full-GET verification fallback when stored bytes are intact")
		}
		if fg2, _, _, _ := be.stats(); fg2 != 1 {
			t.Fatalf("CRC32C mismatch took 0-RPC shortcut instead of verifying via Get: fullGets=%d", fg2)
		}

		// Grow journal via POST /store/append, then restore on a BRAND NEW cold RemoteSource.
		tailLine, _ := journal.Marshal([]journal.Op{{
			Seq: 10001, Lamport: 10001, Time: time.Date(2026, 11, 20, 6, 0, 0, 0, time.UTC),
			Device: "dev1", Kind: journal.KindPut, Path: "notes/cold-tail.md", Blob: blob, Size: 14, Mode: 0o644,
		}})
		u := fmt.Sprintf("/api/p/%s/store/append?key=journal/dev1.jsonl&prev_size=%d&prev_sha256=%s", p.ID, len(raw), qaHexSHA(raw))
		if rec := tailQARequest(t, srv.Handler(), http.MethodPost, u, tailLine, http.Header{"X-Bdrive-Device": {"dev1"}}); rec.Code != http.StatusOK {
			t.Fatalf("append before cold restore: %d %s", rec.Code, rec.Body)
		}

		coldGrownRS := &RemoteSource{Backend: remote.Prefixed(be, p.ID)}
		be.resetStats()
		files2, err := coldGrownRS.Files(ctx)
		if err != nil {
			t.Fatalf("cold restore grown: %v", err)
		}
		if _, ok := files2["notes/cold-tail.md"]; !ok || len(files2) != 10001 {
			t.Fatalf("cold restore grown missing new file: len=%d has=%v", len(files2), ok)
		}
		fg, fb, rg, rb = be.stats()
		if fg != 0 || fb != 0 || rg != 1 {
			t.Fatalf("cold restore grown expected 0 fullGets and 1 rangeGet, got full=%d(%d) range=%d(%d)", fg, fb, rg, rb)
		}
		if wantRB := int64(len(tailLine)) + 1; rb != wantRB {
			t.Fatalf("cold restore grown read %d range bytes, want tail+1 (%d)", rb, wantRB)
		}

		// Tamper the proof byte at old.Size-1 (not '\n') inside an immutable-chain-forged object -> must reject!
		badRaw := append(append([]byte(nil), raw...), tailLine...)
		badRaw[len(raw)-1] = ' '
		be.mu.Lock()
		obj := be.objs[p.ID+"/journal/dev1.jsonl"]
		obj.data = badRaw
		be.objs[p.ID+"/journal/dev1.jsonl"] = obj
		be.mu.Unlock()
		badObj := remote.Object{
			Key: "journal/dev1.jsonl", Size: int64(len(badRaw)), Version: "99", ImmutableVersion: true, CRC32C: obj.crc,
		}
		oldMeta := ckptJournalMeta{Size: int64(len(raw)), PrefixSHA256: qaHexSHA(raw), Version: "1"}
		if _, _, ok := coldGrownRS.verifyAndReadTail(ctx, badObj, oldMeta, true, true); ok {
			t.Fatal("verifyAndReadTail accepted non-newline byte at old.Size-1")
		}
	})

	t.Run("MutableColdRestoreAlwaysVerifiesPrefixBytes", func(t *testing.T) {
		be := newTailTestMetaBackend(false)
		prefBe := remote.Prefixed(be, "p1")
		rs := &RemoteSource{Backend: prefBe}
		raw, _ := seedLargeMetaJournal(t, be, "p1", "dev1", 10000)

		files, err := rs.Files(ctx)
		if err != nil || len(files) != 10000 {
			t.Fatalf("warm files: len=%d err=%v", len(files), err)
		}
		waitForCheckpointWrite(t, rs, be, "p1/"+hubFoldCheckpointKey)

		coldRS := &RemoteSource{Backend: prefBe}
		be.resetStats()
		files2, err := coldRS.Files(ctx)
		if err != nil || len(files2) != 10000 {
			t.Fatalf("mutable cold files: len=%d err=%v", len(files2), err)
		}
		fg, fb, _, _ := be.stats()
		if fg != 1 || fb != int64(len(raw)) {
			t.Fatalf("mutable backend cold restore must verify full prefix, got fullGets=%d bytes=%d", fg, fb)
		}

		// Silent in-place corruption keeping exact size, modtime, and version MUST be detected and trigger full fold fallback.
		be.mu.Lock()
		obj := be.objs["p1/journal/dev1.jsonl"]
		corrupt := append([]byte(nil), obj.data...)
		// Corrupt the opening '{' on the last line to 'X' (invalid JSON, same byte length).
		lastNL := bytes.LastIndexByte(bytes.TrimSuffix(corrupt, []byte{'\n'}), '\n')
		corrupt[lastNL+1] = 'X'
		obj.data = corrupt
		be.objs["p1/journal/dev1.jsonl"] = obj
		be.mu.Unlock()

		coldRS2 := &RemoteSource{Backend: prefBe}
		files3, err := coldRS2.Files(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// One corrupted line is skipped -> 9999 valid ops instead of blindly reusing 10000 from checkpoint!
		if len(files3) != 9999 {
			t.Fatalf("mutable cold restore failed to reject corrupted prefix: files=%d want=9999", len(files3))
		}
	})

	t.Run("NewDeviceJournalAndCounterexampleD", func(t *testing.T) {
		be := newTailTestMetaBackend(true)
		prefBe := remote.Prefixed(be, "p1")
		rs := &RemoteSource{Backend: prefBe}
		_, blob := seedLargeMetaJournal(t, be, "p1", "dev1", 10000)

		if _, err := rs.Files(ctx); err != nil {
			t.Fatal(err)
		}

		// Add small new device journal dev2.
		dev2Line, _ := journal.Marshal([]journal.Op{{
			Seq: 1, Lamport: 20000, Time: time.Date(2026, 11, 20, 7, 0, 0, 0, time.UTC),
			Device: "dev2", Kind: journal.KindPut, Path: "notes/dev2-only.md", Blob: blob, Size: 14, Mode: 0o644,
		}})
		if err := be.Put(ctx, "p1/journal/dev2.jsonl", bytes.NewReader(dev2Line), int64(len(dev2Line))); err != nil {
			t.Fatal(err)
		}
		files, err := rs.Files(ctx)
		if err != nil || files["notes/dev2-only.md"].Blob != blob {
			t.Fatalf("new device journal incremental fold failed: err=%v file=%+v", err, files["notes/dev2-only.md"])
		}

		// Counterexample D: vanished dev2 + added dev3 with equal journal count must fail compatibility check.
		be.mu.Lock()
		cpMeta := cloneJournalMetas(rs.foldJournals)
		be.mu.Unlock()
		objs, err := rs.listJournals(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !compatibleCheckpointJournals(cpMeta, objs) {
			t.Fatal("expected cpMeta compatible with current listing")
		}
		var swapped []remote.Object
		for _, o := range objs {
			if o.Key == "journal/dev2.jsonl" {
				o.Key = "journal/dev3.jsonl"
			}
			swapped = append(swapped, o)
		}
		if compatibleCheckpointJournals(cpMeta, swapped) {
			t.Fatal("Counterexample D: vanished dev2 + new dev3 with equal len passed compatibleCheckpointJournals")
		}

		// New journal > maxIncrementalNewJournalBytes must refuse incremental check.
		oversized := append(append([]remote.Object(nil), objs...), remote.Object{
			Key: "journal/huge.jsonl", Size: maxIncrementalNewJournalBytes + 1,
		})
		if compatibleCheckpointJournals(cpMeta, oversized) {
			t.Fatal("new journal > maxIncrementalNewJournalBytes unexpectedly accepted")
		}
	})
}

func TestQA15_ApplyTailSlicesMoveCounterexamplesAndFuzzOracle(t *testing.T) {
	shaA := strings.Repeat("a", 64)
	shaB := strings.Repeat("b", 64)
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	t.Run("CounterexampleA_SaturationAndZeroTime", func(t *testing.T) {
		// Distant year 0001-01-02 event produces math.MaxInt64/math.MinInt64 duration saturation -> allowed (outside 30s).
		files := map[string]FileInfo{}
		tombs := map[string]winOp{}
		moves := moveIndex{
			"old.md": {{At: time.Date(1, 1, 2, 0, 0, 0, 0, time.UTC)}},
		}
		tail := [][]journal.Op{{{
			Seq: 1, Lamport: 1, Time: t0, Device: "dev1", Kind: journal.KindPut, Path: "new.md", Blob: shaA, Size: 1,
		}}}
		if !applyTailSlices(files, tombs, moves, tail) {
			t.Fatal("valid distant non-zero move event triggered false positive rejection")
		}

		// Zero time in op.Time when moves non-empty -> fail closed!
		files2 := map[string]FileInfo{}
		tombs2 := map[string]winOp{}
		tailZero := [][]journal.Op{{{
			Seq: 1, Lamport: 1, Time: time.Time{}, Device: "dev1", Kind: journal.KindPut, Path: "new.md", Blob: shaA, Size: 1,
		}}}
		if applyTailSlices(files2, tombs2, moves, tailZero) {
			t.Fatal("zero op.Time with non-empty moves must fail closed")
		}

		// Zero Ev.At in moves -> fail closed!
		files3 := map[string]FileInfo{}
		tombs3 := map[string]winOp{}
		movesZero := moveIndex{"old.md": {{At: time.Time{}}}}
		if applyTailSlices(files3, tombs3, movesZero, tail) {
			t.Fatal("zero ev.At in moves must fail closed")
		}
	})

	t.Run("CounterexampleB_SymlinkToBlobNearDelete", func(t *testing.T) {
		files := map[string]FileInfo{
			"p.txt": {Link: "target.txt", Blob: "", Lamport: 1, Seq: 1, DeviceID: "dev1"},
		}
		tombs := map[string]winOp{}
		moves := moveIndex{
			"deleted.txt": {{At: t0}},
		}
		tail := [][]journal.Op{{{
			Seq: 2, Lamport: 2, Time: t0.Add(5 * time.Second), Device: "dev1", Kind: journal.KindPut, Path: "p.txt", Blob: shaA, Size: 10,
		}}}
		if applyTailSlices(files, tombs, moves, tail) {
			t.Fatal("Counterexample B: Link->Blob transition near delete must fail closed")
		}
	})

	t.Run("CounterexampleC_BoundaryAndManyCreateTimes", func(t *testing.T) {
		makeState := func() (map[string]FileInfo, map[string]winOp, moveIndex) {
			return map[string]FileInfo{}, map[string]winOp{}, moveIndex{"gone.md": {{At: t0}}}
		}
		for _, tc := range []struct {
			delta time.Duration
			want  bool
		}{
			{29 * time.Second, false},
			{30 * time.Second, false},
			{31 * time.Second, true},
			{-30 * time.Second, false},
			{-31 * time.Second, true},
		} {
			f, tb, mv := makeState()
			tail := [][]journal.Op{{{
				Seq: 1, Lamport: 1, Time: t0.Add(tc.delta), Device: "dev1", Kind: journal.KindPut, Path: "n.md", Blob: shaA, Size: 1,
			}}}
			if got := applyTailSlices(f, tb, mv, tail); got != tc.want {
				t.Fatalf("delta %v: got %v want %v", tc.delta, got, tc.want)
			}
		}

		// Binary search with >10 create times bracketing ev.At=t0+100s strictly outside +-30s moveWindow.
		f := map[string]FileInfo{}
		tb := map[string]winOp{}
		mv := moveIndex{"del.md": {{At: t0.Add(100 * time.Second)}}}
		var ops []journal.Op
		for i := 0; i < 12; i++ {
			ts := t0.Add(time.Duration(i*10) * time.Second)
			if i >= 6 {
				ts = t0.Add(time.Duration(140+(i-6)*10) * time.Second)
			}
			ops = append(ops, journal.Op{
				Seq: int64(i + 1), Lamport: int64(i + 1), Time: ts, Device: "dev1",
				Kind: journal.KindPut, Path: fmt.Sprintf("f%d.md", i), Blob: shaA, Size: 1,
			})
		}
		if !applyTailSlices(f, tb, mv, [][]journal.Op{ops}) {
			t.Fatal("binary search falsely rejected non-overlapping create times")
		}
	})

	t.Run("RandomizedEquivalenceOracle_500Seeds", func(t *testing.T) {
		blobs := []string{shaA, shaB, strings.Repeat("c", 64)}
		paths := []string{"p0.md", "p1.md", "p2.md", "p3.md", "p4.md", "p5.md"}
		devs := []string{"d1", "d2"}

		moveIndexEqual := func(a, b moveIndex) bool {
			na, nb := 0, 0
			for _, v := range a {
				if len(v) > 0 {
					na++
				}
			}
			for _, v := range b {
				if len(v) > 0 {
					nb++
				}
			}
			if na != nb {
				return false
			}
			for k, evsA := range a {
				if len(evsA) == 0 {
					continue
				}
				if !slices.Equal(evsA, b[k]) {
					return false
				}
			}
			return true
		}

		accepted := 0
		for seed := uint64(1); seed <= 500; seed++ {
			rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
			byDevPre := map[string][]journal.Op{}
			byDevTail := map[string][]journal.Op{}
			seq := map[string]int64{}
			lamport := int64(10)

			// Prefix ops across devices (links, puts, deletes -> exercise moves & tombstones).
			curT := t0
			nPre := 8 + rng.IntN(16)
			for i := 0; i < nPre; i++ {
				dev := devs[rng.IntN(len(devs))]
				seq[dev]++
				lamport += 1 + int64(rng.IntN(5))
				curT = curT.Add(time.Duration(rng.IntN(20)) * time.Second)
				p := paths[rng.IntN(len(paths))]
				op := journal.Op{Seq: seq[dev], Lamport: lamport, Time: curT, Device: dev, Path: p}
				switch rng.IntN(6) {
				case 0:
					op.Kind = journal.KindDelete
				case 1:
					op.Kind = journal.KindPut
					op.Link = "target.md"
				default:
					op.Kind = journal.KindPut
					op.Blob = blobs[rng.IntN(len(blobs))]
					op.Size = 10
				}
				byDevPre[dev] = append(byDevPre[dev], op)
			}

			// Jump well past moveWindow in half the seeds so positive accepts are well-exercised too.
			if rng.IntN(2) == 0 {
				curT = curT.Add(2 * time.Minute)
			}
			nTail := 1 + rng.IntN(5)
			for i := 0; i < nTail; i++ {
				dev := devs[rng.IntN(len(devs))]
				seq[dev]++
				lamport += 1 + int64(rng.IntN(5))
				curT = curT.Add(time.Duration(rng.IntN(15)) * time.Second)
				p := paths[rng.IntN(len(paths))]
				op := journal.Op{Seq: seq[dev], Lamport: lamport, Time: curT, Device: dev, Path: p}
				if rng.IntN(8) == 0 {
					op.Kind = journal.KindDelete
				} else if rng.IntN(8) == 0 {
					op.Kind = journal.KindPut
					op.Link = "link.md"
				} else {
					op.Kind = journal.KindPut
					op.Blob = blobs[rng.IntN(len(blobs))]
					op.Size = 10
				}
				byDevTail[dev] = append(byDevTail[dev], op)
			}

			var preSlices, fullSlices, tailSlices [][]journal.Op
			for _, dev := range devs {
				pre := byDevPre[dev]
				tail := byDevTail[dev]
				if len(pre) > 0 {
					preSlices = append(preSlices, pre)
				}
				if len(tail) > 0 {
					tailSlices = append(tailSlices, tail)
				}
				combined := append(append([]journal.Op(nil), pre...), tail...)
				if len(combined) > 0 {
					fullSlices = append(fullSlices, combined)
				}
			}

			preFiles, preTombs, preMoves := foldParsedJournals(preSlices)

			gotFiles := cloneFiles(preFiles)
			gotTombs := cloneTombstones(preTombs)
			if !applyTailSlices(gotFiles, gotTombs, preMoves, tailSlices) {
				continue
			}
			accepted++

			wantFiles, wantTombs, wantMoves := foldParsedJournals(fullSlices)

			if !reflect.DeepEqual(gotFiles, wantFiles) {
				t.Fatalf("seed %d: files mismatch\ngot=%+v\nwant=%+v", seed, gotFiles, wantFiles)
			}
			if !reflect.DeepEqual(gotTombs, wantTombs) {
				t.Fatalf("seed %d: tombs mismatch\ngot=%+v\nwant=%+v", seed, gotTombs, wantTombs)
			}
			if !moveIndexEqual(preMoves, wantMoves) {
				t.Fatalf("seed %d: moves mismatch\ngot=%+v\nwant=%+v", seed, preMoves, wantMoves)
			}
		}
		if accepted < 50 {
			t.Fatalf("oracle accepted only %d/500 seeds; expected >= 50", accepted)
		}
	})
}
