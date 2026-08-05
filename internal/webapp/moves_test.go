package webapp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// t0 is the fixed clock these tests place ops on. Every pairing rule in
// moves.go is time-windowed, so "now" would make them unreadable.
var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// delAt is the timed twin of fakeRemote.del (putAt already lives in
// history_test.go).
func (f *fakeRemote) delAt(dev, path string, at time.Time) {
	f.t.Helper()
	f.append(dev, journal.Op{Kind: journal.KindDelete, Path: path, Time: at})
}

// move is the shape the scanner actually emits: one device, one cycle, the
// same blob put at the new path and deleted at the old.
func (f *fakeRemote) move(dev, from, to, content string, at time.Time) {
	f.t.Helper()
	f.putAt(dev, to, content, at)
	f.delAt(dev, from, at.Add(time.Second))
}

/* ---- the index ---- */

// buildIndex is the unit-level path: ops in, index out.
func buildIndex(ops ...journal.Op) moveIndex {
	journal.Sort(ops)
	return buildMoveIndex(ops)
}

func op(kind, path, blob string, at time.Time, dev string, lamport int64) journal.Op {
	return journal.Op{
		Kind: kind, Path: path, Blob: blob, Time: at, Device: dev,
		Lamport: lamport, Seq: lamport, Size: 4, Mode: 0o644,
	}
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestMoveIndexPairsInEitherJournalOrder(t *testing.T) {
	b := sha("body")
	for _, tc := range []struct {
		name string
		ops  []journal.Op
	}{
		{"put-then-delete", []journal.Op{
			op(journal.KindPut, "a.md", b, t0, "deva", 1),
			op(journal.KindPut, "docs/a.md", b, t0.Add(time.Minute), "deva", 2),
			op(journal.KindDelete, "a.md", "", t0.Add(time.Minute+time.Second), "deva", 3),
		}},
		{"delete-then-put", []journal.Op{
			op(journal.KindPut, "a.md", b, t0, "deva", 1),
			op(journal.KindDelete, "a.md", "", t0.Add(time.Minute), "deva", 2),
			op(journal.KindPut, "docs/a.md", b, t0.Add(time.Minute+4*time.Second), "deva", 3),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := buildIndex(tc.ops...)
			evs := idx["a.md"]
			if len(evs) != 1 || evs[0].To != "docs/a.md" {
				t.Fatalf("a.md events = %+v, want one move to docs/a.md", evs)
			}
		})
	}
}

func TestMoveIndexDeclinesOutsideWindow(t *testing.T) {
	b := sha("body")
	idx := buildIndex(
		op(journal.KindPut, "a.md", b, t0, "deva", 1),
		op(journal.KindDelete, "a.md", "", t0.Add(time.Minute), "deva", 2),
		op(journal.KindPut, "docs/a.md", b, t0.Add(time.Hour), "deva", 3),
	)
	if evs := idx["a.md"]; len(evs) != 1 || evs[0].To != "" {
		t.Fatalf("a.md events = %+v, want one plain deletion", evs)
	}
}

func TestMoveIndexDeclinesAcrossDevices(t *testing.T) {
	b := sha("body")
	idx := buildIndex(
		op(journal.KindPut, "a.md", b, t0, "deva", 1),
		op(journal.KindDelete, "a.md", "", t0.Add(time.Minute), "deva", 2),
		op(journal.KindPut, "docs/a.md", b, t0.Add(time.Minute+time.Second), "devb", 3),
	)
	if evs := idx["a.md"]; len(evs) != 1 || evs[0].To != "" {
		t.Fatalf("a.md events = %+v, want one plain deletion", evs)
	}
}

func TestMoveIndexAmbiguousIdenticalContent(t *testing.T) {
	// Two files with the same bytes, one deleted, one created: content
	// identity cannot say which is which, so nothing pairs.
	b := sha("body")
	idx := buildIndex(
		op(journal.KindPut, "a.md", b, t0, "deva", 1),
		op(journal.KindPut, "b.md", b, t0, "deva", 2),
		op(journal.KindDelete, "a.md", "", t0.Add(time.Minute), "deva", 3),
		op(journal.KindDelete, "b.md", "", t0.Add(time.Minute), "deva", 4),
		op(journal.KindPut, "docs/a.md", b, t0.Add(time.Minute+time.Second), "deva", 5),
	)
	for _, p := range []string{"a.md", "b.md"} {
		if evs := idx[p]; len(evs) != 1 || evs[0].To != "" {
			t.Fatalf("%s events = %+v, want a plain deletion (ambiguous)", p, evs)
		}
	}
}

func TestMoveIndexIgnoresOverwritingPut(t *testing.T) {
	// docs/a.md already existed, so the put that happens to carry a.md's
	// blob is an edit, not a move destination.
	b := sha("body")
	idx := buildIndex(
		op(journal.KindPut, "a.md", b, t0, "deva", 1),
		op(journal.KindPut, "docs/a.md", sha("other"), t0, "deva", 2),
		op(journal.KindDelete, "a.md", "", t0.Add(time.Minute), "deva", 3),
		op(journal.KindPut, "docs/a.md", b, t0.Add(time.Minute), "deva", 4),
	)
	if evs := idx["a.md"]; len(evs) != 1 || evs[0].To != "" {
		t.Fatalf("a.md events = %+v, want a plain deletion", evs)
	}
}

/* ---- resolvers ---- */

func TestResolveShareDeleteBeforeMove(t *testing.T) {
	// a.md is deleted, an unrelated a.md is created at the same address,
	// and THAT one moves away. A share minted before the delete is a promise
	// about the first file — which is gone.
	b, c := sha("first"), sha("second")
	idx := buildIndex(
		op(journal.KindPut, "a.md", b, t0, "deva", 1),
		op(journal.KindDelete, "a.md", "", t0.Add(time.Hour), "deva", 2),
		op(journal.KindPut, "a.md", c, t0.Add(2*time.Hour), "deva", 3),
		op(journal.KindPut, "docs/a.md", c, t0.Add(3*time.Hour), "deva", 4),
		op(journal.KindDelete, "a.md", "", t0.Add(3*time.Hour+time.Second), "deva", 5),
	)
	files := map[string]FileInfo{"docs/a.md": {Blob: c}}
	if to, ok := resolveShare(idx, files, "a.md", t0); ok {
		t.Fatalf("share minted before the delete resolved to %q, want gone", to)
	}
	to, ok := resolveShare(idx, files, "a.md", t0.Add(2*time.Hour+time.Minute))
	if !ok || to != "docs/a.md" {
		t.Fatalf("share minted after the recreate = %q %v, want docs/a.md", to, ok)
	}
}

func TestResolveForwardCycle(t *testing.T) {
	// A journal is peer JSON and can describe A→B→A. Answer "not found"
	// rather than walk forever.
	idx := moveIndex{
		"a.md": {{At: t0, To: "b.md", ToAt: t0}},
		"b.md": {{At: t0.Add(time.Minute), To: "a.md", ToAt: t0.Add(time.Minute)}},
	}
	if to, ok := resolveForward(idx, map[string]FileInfo{}, "a.md"); ok {
		t.Fatalf("cycle resolved to %q, want not found", to)
	}
	if to, ok := resolveShare(idx, map[string]FileInfo{}, "a.md", time.Time{}); ok {
		t.Fatalf("cycle resolved (share) to %q, want not found", to)
	}
}

func TestChainSegmentsExcludeSuccessorAtOldPath(t *testing.T) {
	b := sha("body")
	idx := buildIndex(
		op(journal.KindPut, "a.md", b, t0, "deva", 1),
		op(journal.KindPut, "docs/a.md", b, t0.Add(time.Hour), "deva", 2),
		op(journal.KindDelete, "a.md", "", t0.Add(time.Hour+time.Second), "deva", 3),
		op(journal.KindPut, "a.md", sha("unrelated"), t0.Add(2*time.Hour), "deva", 4),
	)
	segs := chainSegments(idx, "docs/a.md")
	if !inSegments(segs, "a.md", t0) {
		t.Error("the original a.md version is not in docs/a.md's chain")
	}
	if inSegments(segs, "a.md", t0.Add(2*time.Hour)) {
		t.Error("the unrelated later a.md leaked into docs/a.md's chain")
	}
	if !inSegments(segs, "docs/a.md", t0.Add(time.Hour)) {
		t.Error("docs/a.md's own creating op is not in its chain")
	}
}

/* ---- viewer ---- */

func TestMovedFileRedirects(t *testing.T) {
	f := newFakeRemote(t)
	f.putAt("deva", "a.md", "# Body", t0)
	f.move("deva", "a.md", "docs/a.md", "# Body", t0.Add(time.Hour))
	h := f.server().Handler()

	rec := get(t, h, "/api/file?path=a.md")
	if rec.Code != 200 || rec.Body.String() != "# Body" {
		t.Fatalf("moved file: %d %q", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("X-Bdrive-Canonical-Path"); got != "docs/a.md" {
		t.Fatalf("canonical header = %q, want docs/a.md", got)
	}
	// The destination itself carries no header — nothing moved.
	rec = get(t, h, "/api/file?path=docs/a.md")
	if got := rec.Header().Get("X-Bdrive-Canonical-Path"); got != "" {
		t.Fatalf("canonical header on the live path = %q, want none", got)
	}
	// A path that never existed still 404s.
	if rec := get(t, h, "/api/file?path=nope.md"); rec.Code != 404 {
		t.Fatalf("unknown path: %d, want 404", rec.Code)
	}
}

func TestLivePathWinsOverRedirect(t *testing.T) {
	f := newFakeRemote(t)
	f.putAt("deva", "a.md", "# Body", t0)
	f.move("deva", "a.md", "docs/a.md", "# Body", t0.Add(time.Hour))
	f.putAt("deva", "a.md", "# Brand new", t0.Add(2*time.Hour))
	h := f.server().Handler()

	rec := get(t, h, "/api/file?path=a.md")
	if rec.Code != 200 || rec.Body.String() != "# Brand new" {
		t.Fatalf("live path: %d %q, want the new file", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("X-Bdrive-Canonical-Path"); got != "" {
		t.Fatalf("canonical header = %q, want none (nothing moved out of a live path)", got)
	}
}

func TestMoveChainResolvesInOneHop(t *testing.T) {
	f := newFakeRemote(t)
	f.putAt("deva", "a.md", "body", t0)
	f.move("deva", "a.md", "b.md", "body", t0.Add(time.Hour))
	f.move("deva", "b.md", "c.md", "body", t0.Add(2*time.Hour))
	h := f.server().Handler()

	rec := get(t, h, "/api/file?path=a.md")
	if rec.Code != 200 {
		t.Fatalf("a.md: %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("X-Bdrive-Canonical-Path"); got != "c.md" {
		t.Fatalf("canonical header = %q, want c.md", got)
	}
}

func TestRenderFollowsMove(t *testing.T) {
	f := newFakeRemote(t)
	f.putAt("deva", "a.md", "# Title", t0)
	f.move("deva", "a.md", "docs/a.md", "# Title", t0.Add(time.Hour))
	h := f.server().Handler()

	rec := get(t, h, "/api/render?path=a.md")
	if rec.Code != 200 {
		t.Fatalf("render: %d %s", rec.Code, rec.Body)
	}
	var doc struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Path != "docs/a.md" {
		t.Fatalf("rendered path = %q, want the canonical docs/a.md", doc.Path)
	}
	if got := rec.Header().Get("X-Bdrive-Canonical-Path"); got != "docs/a.md" {
		t.Fatalf("canonical header = %q", got)
	}
}

/* ---- /resolve ---- */

func TestResolveEndpoint(t *testing.T) {
	f := newFakeRemote(t)
	f.putAt("deva", "notes/one.md", "one", t0)
	f.putAt("deva", "notes/two.md", "two", t0)
	f.move("deva", "notes/one.md", "wiki/one.md", "one", t0.Add(time.Hour))
	f.move("deva", "notes/two.md", "wiki/two.md", "two", t0.Add(time.Hour))
	h := f.server().Handler()

	var got struct{ To, Kind string }
	rec := get(t, h, "/api/resolve?path=notes")
	if rec.Code != 200 {
		t.Fatalf("folder resolve: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.To != "wiki" || got.Kind != "folder" {
		t.Fatalf("folder resolve = %+v, want wiki/folder", got)
	}

	rec = get(t, h, "/api/resolve?path=notes/one.md")
	if rec.Code != 200 {
		t.Fatalf("file resolve: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.To != "wiki/one.md" || got.Kind != "file" {
		t.Fatalf("file resolve = %+v", got)
	}

	// A live path has not gone anywhere.
	if rec := get(t, h, "/api/resolve?path=wiki/one.md"); rec.Code != 404 {
		t.Fatalf("live path resolve: %d, want 404", rec.Code)
	}
}

func TestResolveFolderIsAllOrNothing(t *testing.T) {
	t.Run("half moved", func(t *testing.T) {
		f := newFakeRemote(t)
		f.putAt("deva", "notes/one.md", "one", t0)
		f.putAt("deva", "notes/two.md", "two", t0)
		f.move("deva", "notes/one.md", "wiki/one.md", "one", t0.Add(time.Hour))
		if rec := get(t, f.server().Handler(), "/api/resolve?path=notes"); rec.Code != 404 {
			t.Fatalf("partial move: %d, want 404 (notes/ still exists)", rec.Code)
		}
	})
	t.Run("one deleted", func(t *testing.T) {
		f := newFakeRemote(t)
		f.putAt("deva", "notes/one.md", "one", t0)
		f.putAt("deva", "notes/two.md", "two", t0)
		f.move("deva", "notes/one.md", "wiki/one.md", "one", t0.Add(time.Hour))
		f.delAt("deva", "notes/two.md", t0.Add(2*time.Hour))
		if rec := get(t, f.server().Handler(), "/api/resolve?path=notes"); rec.Code != 404 {
			t.Fatalf("deleted member: %d, want 404 (no honest destination)", rec.Code)
		}
	})
	t.Run("split destinations", func(t *testing.T) {
		f := newFakeRemote(t)
		f.putAt("deva", "notes/one.md", "one", t0)
		f.putAt("deva", "notes/two.md", "two", t0)
		f.move("deva", "notes/one.md", "wiki/one.md", "one", t0.Add(time.Hour))
		f.move("deva", "notes/two.md", "docs/two.md", "two", t0.Add(time.Hour))
		if rec := get(t, f.server().Handler(), "/api/resolve?path=notes"); rec.Code != 404 {
			t.Fatalf("split destinations: %d, want 404", rec.Code)
		}
	})
}

/* ---- read heat lands on the canonical path ---- */

func TestRedirectedReadCreditsCanonicalPath(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	var err error
	if srv.Reads, err = OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0); err != nil {
		t.Fatal(err)
	}
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.putAt("deva", "a.md", "# Body", t0)
	f.move("deva", "a.md", "docs/a.md", "# Body", t0.Add(time.Hour))
	h := srv.Handler()

	if rec := do(t, h, "GET", "/api/p/"+p.ID+"/file?path=a.md", nil); rec.Code != 200 {
		t.Fatalf("read: %d %s", rec.Code, rec.Body)
	}
	rec := do(t, h, "GET", "/api/p/"+p.ID+"/heat?days=30", nil)
	var heat struct {
		Entries map[string]HeatEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &heat); err != nil {
		t.Fatalf("heat: %v (%s)", err, rec.Body)
	}
	if heat.Entries["docs/a.md"].Human == 0 {
		t.Errorf("heat credited %v, want the read on docs/a.md", heat.Entries)
	}
	if heat.Entries["a.md"].Human != 0 {
		t.Errorf("heat credited the OLD path a.md: %v", heat.Entries)
	}
}
