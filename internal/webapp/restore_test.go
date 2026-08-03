package webapp

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
)

// journalsAt reads every journal file under a project's storage prefix, so a
// test can prove a write touched only the server's own.
func journalsAt(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, "journal", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

func historyOf(t *testing.T, h http.Handler, base, path string) []HistoryEntry {
	t.Helper()
	rec := do(t, h, "GET", base+"history?path="+path, nil)
	var out struct {
		Entries []HistoryEntry `json:"entries"`
	}
	mustJSON(t, rec, &out)
	return out.Entries
}

// Restoring appends a new put pointing at the old blob: the file serves the
// historical content again, and the restore itself is visible in history.
func TestRestoreWritesNewOp(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	dir := filepath.Join(root, p.ID)
	f := newFakeRemoteAt(t, dir)
	f.put("dev1", "notes/plan.md", "v1")
	f.put("dev1", "notes/plan.md", "v2 longer")
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"

	rec := do(t, h, "POST", base+"restore", map[string]string{"path": "notes/plan.md", "sha": shaOf("v1")})
	var out struct {
		OK   bool   `json:"ok"`
		Blob string `json:"blob"`
		Size int64  `json:"size"`
	}
	mustJSON(t, rec, &out)
	if !out.OK || out.Blob != shaOf("v1") || out.Size != int64(len("v1")) {
		t.Fatalf("restore response = %+v", out)
	}

	if rec := do(t, h, "GET", base+"file?path=notes/plan.md", nil); rec.Body.String() != "v1" {
		t.Fatalf("file after restore = %q, want v1", rec.Body)
	}
	entries := historyOf(t, h, base, "notes/plan.md")
	if len(entries) != 3 {
		t.Fatalf("history = %d entries, want 3", len(entries))
	}
	newest := entries[0]
	want := "restore notes/plan.md@" + shaOf("v1")[:8]
	if newest.Note != want {
		t.Fatalf("restore note = %q, want %q", newest.Note, want)
	}
	if newest.Blob != shaOf("v1") || newest.Size != int64(len("v1")) {
		t.Fatalf("restore entry = %+v", newest)
	}
}

// A blob that exists but was never a version of THIS path must not be
// pasteable onto it.
func TestRestoreUnknownVersion(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	dir := filepath.Join(root, p.ID)
	f := newFakeRemoteAt(t, dir)
	f.put("dev1", "a.md", "mine")
	f.put("dev1", "b.md", "someone else's")
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"
	before := journalsAt(t, dir)

	for _, sha := range []string{shaOf("b.md content that is elsewhere"), shaOf("someone else's")} {
		rec := do(t, h, "POST", base+"restore", map[string]string{"path": "a.md", "sha": sha})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("restore of a foreign blob: %d %s, want 404", rec.Code, rec.Body)
		}
	}
	// a malformed sha never reaches the journals either
	if rec := do(t, h, "POST", base+"restore", map[string]string{"path": "a.md", "sha": "nope"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad sha: %d, want 400", rec.Code)
	}
	if got := journalsAt(t, dir); len(got) != len(before) {
		t.Fatalf("a refused restore wrote a journal: %v → %v", before, got)
	}
	for name, data := range before {
		if got := journalsAt(t, dir)[name]; got != data {
			t.Fatalf("journal %s changed on a refused restore", name)
		}
	}
}

// Restoring the version the file ALREADY has is not a change — it would
// journal a +0 −0 row and sync it to every device. The guard is narrow: the
// version below it still restores.
func TestRestoreNoOpCurrentVersion(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	dir := filepath.Join(root, p.ID)
	f := newFakeRemoteAt(t, dir)
	f.put("dev1", "f.md", "v1")
	f.put("dev1", "f.md", "v2")
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"
	before := journalsAt(t, dir)

	rec := do(t, h, "POST", base+"restore", map[string]string{"path": "f.md", "sha": shaOf("v2")})
	if rec.Code != http.StatusConflict {
		t.Fatalf("restore of the current version: %d %s, want 409", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "already the current content") {
		t.Fatalf("409 body = %q", rec.Body)
	}
	after := journalsAt(t, dir)
	if len(after) != len(before) {
		t.Fatalf("a refused restore wrote a journal: %v → %v", before, after)
	}
	for name, data := range before {
		if after[name] != data {
			t.Fatalf("journal %s changed on a refused restore", name)
		}
	}

	// the older version is still restorable — and once it is the current
	// content, restoring IT becomes the no-op instead.
	if rec := do(t, h, "POST", base+"restore", map[string]string{"path": "f.md", "sha": shaOf("v1")}); rec.Code != 200 {
		t.Fatalf("restore of an older version: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", base+"restore", map[string]string{"path": "f.md", "sha": shaOf("v1")}); rec.Code != http.StatusConflict {
		t.Fatalf("re-restore of what is now current: %d %s, want 409", rec.Code, rec.Body)
	}
}

// One writer per journal: the hub appends to its own key and to nothing else.
func TestRestoreOnlyTouchesOwnJournal(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	dir := filepath.Join(root, p.ID)
	f := newFakeRemoteAt(t, dir)
	f.put("dev1", "f.md", "v1")
	f.put("dev2", "f.md", "v2")
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"
	before := journalsAt(t, dir)

	rec := do(t, h, "POST", base+"restore", map[string]string{"path": "f.md", "sha": shaOf("v1")})
	if rec.Code != 200 {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	after := journalsAt(t, dir)
	for _, dev := range []string{"dev1.jsonl", "dev2.jsonl"} {
		if after[dev] != before[dev] {
			t.Fatalf("restore rewrote %s", dev)
		}
	}
	own := after[webDevice.ID+".jsonl"]
	if own == "" || !strings.HasPrefix(own, before[webDevice.ID+".jsonl"]) {
		t.Fatal("the server's own journal was not appended to")
	}
	ops, err := journal.Parse([]byte(own))
	if err != nil || len(ops) != 1 {
		t.Fatalf("own journal ops = %d (%v), want 1", len(ops), err)
	}
}

// A blocked plan blocks restores too, even though a restore stores no new
// bytes — and nothing is journaled when it does.
func TestRestoreQuotaBlocked(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	dir := filepath.Join(root, p.ID)
	f := newFakeRemoteAt(t, dir)
	f.put("dev1", "f.md", "v1")
	f.put("dev1", "f.md", "v2")
	q := &recQuota{denyW: true}
	srv.Quota = q
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"
	before := journalsAt(t, dir)

	rec := do(t, h, "POST", base+"restore", map[string]string{"path": "f.md", "sha": shaOf("v1")})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("blocked restore: %d %s, want 413", rec.Code, rec.Body)
	}
	if len(q.usage) != 0 {
		t.Fatalf("a denied restore recorded usage: %+v", q.usage)
	}
	if len(q.writes) != 1 || q.writes[0].bytes != 0 {
		t.Fatalf("CheckWrite calls = %+v, want one for 0 bytes", q.writes)
	}
	for name, data := range journalsAt(t, dir) {
		if data != before[name] {
			t.Fatalf("denied restore wrote journal %s", name)
		}
	}

	// allowed again: it goes through, and records zero bytes
	q.denyW = false
	if rec := do(t, h, "POST", base+"restore", map[string]string{"path": "f.md", "sha": shaOf("v1")}); rec.Code != 200 {
		t.Fatalf("restore after unblocking: %d %s", rec.Code, rec.Body)
	}
	if len(q.usage) != 1 || q.usage[0].bytes != 0 {
		t.Fatalf("RecordUsage = %+v, want one call for 0 bytes", q.usage)
	}
}

// A deleted file comes back: the blob outlives the delete op.
func TestRestoreAfterDelete(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	dir := filepath.Join(root, p.ID)
	f := newFakeRemoteAt(t, dir)
	f.put("dev1", "gone.md", "still here")
	f.del("dev1", "gone.md")
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"

	if rec := do(t, h, "GET", base+"tree", nil); strings.Contains(rec.Body.String(), "gone.md") {
		t.Fatal("file should be deleted before the restore")
	}
	rec := do(t, h, "POST", base+"restore", map[string]string{"path": "gone.md", "sha": shaOf("still here")})
	if rec.Code != 200 {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", base+"tree", nil); !strings.Contains(rec.Body.String(), "gone.md") {
		t.Fatalf("file did not come back: %s", rec.Body)
	}
	if rec := do(t, h, "GET", base+"file?path=gone.md", nil); rec.Body.String() != "still here" {
		t.Fatalf("restored content = %q", rec.Body)
	}
	// The no-op guard must not fire on a deleted path: replay drops it, so
	// there is no current content for the blob to equal. Now that the file is
	// back, though, the same call IS a no-op.
	if rec := do(t, h, "POST", base+"restore", map[string]string{"path": "gone.md", "sha": shaOf("still here")}); rec.Code != http.StatusConflict {
		t.Fatalf("restore of the now-current content: %d %s, want 409", rec.Code, rec.Body)
	}
}

// Restore is a write: a hub running without --upload stays read-only.
func TestRestoreNeedsUploadsEnabled(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("dev1", "f.md", "v1")
	h := srv.Handler()

	rec := do(t, h, "POST", "/api/p/"+p.ID+"/restore", map[string]string{"path": "f.md", "sha": shaOf("v1")})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("restore on a read-only hub: %d %s, want 403", rec.Code, rec.Body)
	}
}

// The single-volume (plain folder) server has no journal to look a version
// up in, so it has no restore route at all.
func TestRestoreNotOnSingleVolume(t *testing.T) {
	f := newFakeRemote(t)
	f.put("dev1", "f.md", "v1")
	h := f.uploadServer(nil).Handler()
	rec := do(t, h, "POST", "/api/restore", map[string]string{"path": "f.md", "sha": shaOf("v1")})
	if rec.Code < 400 {
		t.Fatalf("single-volume restore: %d, want no such route", rec.Code)
	}
}
