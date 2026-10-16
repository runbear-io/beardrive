package webapp

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// randomOps builds journals that exercise everything the fold looks at: ties
// under journal.Less (shared lamports and times across devices, and exact
// duplicates that differ only in fields Less ignores), deletes, recreates,
// moves inside and outside the pairing window, invalid blobs and links.
func randomOps(r *rand.Rand, n int) []journal.Op {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	paths := []string{"a.md", "b.md", "c.md", "d/e.md", "d/f.md", "g.md"}
	devs := []string{"dev-1", "dev-2", "dev-3"}
	blobs := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), "not-a-sha", ""}
	var ops []journal.Op
	for i := 0; i < n; i++ {
		op := journal.Op{
			Seq:     int64(r.Intn(n)),
			Lamport: int64(r.Intn(n / 4)),
			Time:    base.Add(time.Duration(r.Intn(120)) * time.Second),
			Device:  devs[r.Intn(len(devs))],
			Path:    paths[r.Intn(len(paths))],
			Kind:    journal.KindPut,
			Blob:    blobs[r.Intn(len(blobs))],
			Size:    int64(r.Intn(3)),
			User:    fmt.Sprintf("u%d", r.Intn(3)),
		}
		switch r.Intn(6) {
		case 0:
			op.Kind, op.Blob = journal.KindDelete, ""
		case 1:
			op.Blob, op.Link = "", "target-"+op.Path
		}
		ops = append(ops, op)
		if r.Intn(5) == 0 {
			dup := op
			dup.UserName = "a duplicate Less cannot tell apart"
			ops = append(ops, dup)
		}
	}
	return ops
}

// The fold groups ops by path instead of sorting the whole project. It must
// produce exactly what a global journal.Sort did, or the viewer, share links
// and comment placement would disagree with what devices converge to.
func TestByPathOrderFoldsLikeAGlobalSort(t *testing.T) {
	for seed := int64(1); seed <= 300; seed++ {
		r := rand.New(rand.NewSource(seed))
		ops := randomOps(r, 10+r.Intn(200))

		global := append([]journal.Op(nil), ops...)
		journal.Sort(global)
		wantFiles, wantMoves := foldFiles(global), buildMoveIndex(global)

		// Split into journals the way the hub reads them: the global sort sees
		// their concatenation, so ties must resolve to journal order there too.
		cut1, cut2 := len(ops)/3, 2*len(ops)/3
		grouped := byPathOrder(ops[:cut1], ops[cut1:cut2], ops[cut2:])
		gotFiles, gotMoves := foldFiles(grouped), buildMoveIndex(grouped)

		if !reflect.DeepEqual(gotFiles, wantFiles) {
			t.Fatalf("seed %d: files differ\n got %+v\nwant %+v", seed, gotFiles, wantFiles)
		}
		if !reflect.DeepEqual(gotMoves, wantMoves) {
			t.Fatalf("seed %d: moves differ\n got %+v\nwant %+v", seed, gotMoves, wantMoves)
		}
	}
}

// An idle project's refresh must not re-fold: an unchanged journal listing
// returns the previous fold, and a journal that grows is folded again.
func TestFilesWithMovesReusesAnUnchangedFold(t *testing.T) {
	ctx := context.Background()
	be, err := remote.Open(ctx, "file://"+t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	put := func(lines ...string) {
		body := strings.Join(lines, "\n") + "\n"
		if err := be.Put(ctx, "journal/dev-1.jsonl", strings.NewReader(body), int64(len(body))); err != nil {
			t.Fatal(err)
		}
	}
	line := func(seq int, path string) string {
		return fmt.Sprintf(`{"seq":%d,"lamport":%d,"time":"2026-01-01T00:00:0%dZ","device":"dev-1","kind":"put","path":%q,"blob":%q,"size":1}`,
			seq, seq, seq, path, strings.Repeat("a", 64))
	}
	put(line(1, "a.md"))
	rs := &RemoteSource{Backend: be}

	first, _, err := rs.FilesWithMoves(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := rs.FilesWithMoves(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.ValueOf(first).Pointer() != reflect.ValueOf(again).Pointer() {
		t.Fatal("an unchanged journal listing was folded again")
	}

	put(line(1, "a.md"), line(2, "b.md"))
	grown, _, err := rs.FilesWithMoves(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := grown["b.md"]; !ok || len(grown) != 2 {
		t.Fatalf("a grown journal was not re-folded: %v", grown)
	}
}

func TestIsBlobSHAMatchesBlobRe(t *testing.T) {
	for _, s := range []string{
		strings.Repeat("a", 64), strings.Repeat("0", 64), strings.Repeat("f", 64),
		strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("a", 63), strings.Repeat("a", 65),
		strings.Repeat("a", 63) + "\n", "", "../" + strings.Repeat("a", 61), strings.Repeat("a", 32) + "/" + strings.Repeat("a", 31),
	} {
		if got, want := isBlobSHA(s), blobRe.MatchString(s); got != want {
			t.Errorf("isBlobSHA(%q) = %v, blobRe says %v", s, got, want)
		}
	}
}

// The tree is encoded once per snapshot and served compressed as stored; it
// must still be the same tree, honor If-None-Match, and pick up a journal that
// grew.
func TestTreeIsEncodedOncePerSnapshot(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	h := srv.Handler()
	journalPath := filepath.Join(root, p.ID, "journal", "dev-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(journalPath), 0o755); err != nil {
		t.Fatal(err)
	}
	line := func(seq int, path string) string {
		return fmt.Sprintf(`{"seq":%d,"lamport":%d,"time":"2026-01-01T00:00:0%dZ","device":"dev-1","kind":"put","path":%q,"blob":%q,"size":1}`+"\n",
			seq, seq, seq, path, strings.Repeat("a", 64))
	}
	write := func(body string) {
		if err := os.WriteFile(journalPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	get := func(etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/p/"+p.ID+"/tree?slim=1", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	names := func(rec *httptest.ResponseRecorder) []string {
		t.Helper()
		if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("tree: %d, encoding %q", rec.Code, rec.Header().Get("Content-Encoding"))
		}
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatal(err)
		}
		var tree Node
		if err := json.NewDecoder(zr).Decode(&tree); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range tree.Children {
			out = append(out, c.Name)
		}
		return out
	}

	write(line(1, "a.md"))
	first := get("")
	if got := names(first); !reflect.DeepEqual(got, []string{"a.md"}) {
		t.Fatalf("tree = %v", got)
	}
	_, vol, _ := srv.projectVolume(p.ID)
	snap, _ := vol.snapshot(context.Background())
	enc, _ := snap.fullTree(true)

	if rec := get(first.Header().Get("ETag")); rec.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d, want 304", rec.Code)
	}
	get("")
	snap2, _ := vol.snapshot(context.Background())
	if again, _ := snap2.fullTree(true); again != enc {
		t.Fatal("an unchanged project re-encoded its tree")
	}

	write(line(1, "a.md") + line(2, "b.md"))
	if got := names(get("")); !reflect.DeepEqual(got, []string{"a.md", "b.md"}) {
		t.Fatalf("after the journal grew, tree = %v", got)
	}
}
