package webapp

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Removing a file journals one delete op under the hub's own device: the file
// leaves the tree, history gains a delete row, and no other device's journal
// is touched.
func TestRemoveWritesDeleteOp(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	dir := filepath.Join(root, p.ID)
	f := newFakeRemoteAt(t, dir)
	f.put("dev1", "ideas.md", "an agent wrote this")
	f.put("dev2", "keep.md", "untouched")
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"
	before := journalsAt(t, dir)

	rec := do(t, h, "POST", base+"remove", map[string]string{"path": "ideas.md"})
	var out struct {
		OK   bool   `json:"ok"`
		Path string `json:"path"`
	}
	mustJSON(t, rec, &out)
	if !out.OK || out.Path != "ideas.md" {
		t.Fatalf("remove response = %+v", out)
	}

	if rec := do(t, h, "GET", base+"tree", nil); strings.Contains(rec.Body.String(), "ideas.md") {
		t.Fatalf("removed file still in the tree: %s", rec.Body)
	}
	entries := historyOf(t, h, base, "ideas.md")
	if len(entries) != 2 {
		t.Fatalf("history = %d entries, want 2", len(entries))
	}
	if newest := entries[0]; newest.Kind != string(journal.KindDelete) || newest.Note != "remove ideas.md" {
		t.Fatalf("newest entry = %+v, want a delete noted %q", newest, "remove ideas.md")
	}

	// One writer per journal: only our own key grew, by exactly one op.
	after := journalsAt(t, dir)
	for _, dev := range []string{"dev1.jsonl", "dev2.jsonl"} {
		if after[dev] != before[dev] {
			t.Fatalf("remove rewrote %s", dev)
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
	if op := ops[0]; op.Kind != journal.KindDelete || op.Path != "ideas.md" || op.Blob != "" || op.Size != 0 {
		t.Fatalf("journaled op = %+v, want a contentless delete of ideas.md", op)
	}
}

// A path that is not in the current state is a 404, and writes nothing.
func TestRemoveUnknownPath(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	dir := filepath.Join(root, p.ID)
	f := newFakeRemoteAt(t, dir)
	f.put("dev1", "here.md", "v1")
	f.put("dev1", "gone.md", "v1")
	f.del("dev1", "gone.md")
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"
	before := journalsAt(t, dir)

	// never existed, and already deleted — both are "not a file right now"
	for _, path := range []string{"nope.md", "gone.md"} {
		rec := do(t, h, "POST", base+"remove", map[string]string{"path": path})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("remove %q: %d %s, want 404", path, rec.Code, rec.Body)
		}
	}
	assertJournalsUnchanged(t, dir, before)
}

// Path validation is the upload validator, so traversal and reserved names
// never reach the journal.
func TestRemoveBadPath(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	dir := filepath.Join(root, p.ID)
	f := newFakeRemoteAt(t, dir)
	f.put("dev1", "f.md", "v1")
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"
	before := journalsAt(t, dir)

	for _, path := range []string{"", "../x", "/abs", "x/", ".bdrive", "a/../b"} {
		rec := do(t, h, "POST", base+"remove", map[string]string{"path": path})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("remove %q: %d %s, want 400", path, rec.Code, rec.Body)
		}
	}
	assertJournalsUnchanged(t, dir, before)
}

// Remove is a write: a hub running without --upload stays read-only.
func TestRemoveNeedsUploadsEnabled(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("dev1", "f.md", "v1")
	h := srv.Handler()

	rec := do(t, h, "POST", "/api/p/"+p.ID+"/remove", map[string]string{"path": "f.md"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remove on a read-only hub: %d %s, want 403", rec.Code, rec.Body)
	}
}

// A blocked plan blocks removes too, even though a delete stores no bytes.
func TestRemoveQuotaBlocked(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	dir := filepath.Join(root, p.ID)
	f := newFakeRemoteAt(t, dir)
	f.put("dev1", "f.md", "v1")
	q := &recQuota{denyW: true}
	srv.Quota = q
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"
	before := journalsAt(t, dir)

	rec := do(t, h, "POST", base+"remove", map[string]string{"path": "f.md"})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("blocked remove: %d %s, want 413", rec.Code, rec.Body)
	}
	if len(q.usage) != 0 {
		t.Fatalf("a denied remove recorded usage: %+v", q.usage)
	}
	if len(q.writes) != 1 || q.writes[0].bytes != 0 {
		t.Fatalf("CheckWrite calls = %+v, want one for 0 bytes", q.writes)
	}
	assertJournalsUnchanged(t, dir, before)

	q.denyW = false
	if rec := do(t, h, "POST", base+"remove", map[string]string{"path": "f.md"}); rec.Code != 200 {
		t.Fatalf("remove after unblocking: %d %s", rec.Code, rec.Body)
	}
	if len(q.usage) != 1 || q.usage[0].bytes != 0 {
		t.Fatalf("RecordUsage = %+v, want one call for 0 bytes", q.usage)
	}
}

// The round trip the UI promises: undo removes the file, and the DELETED row
// it leaves behind restores the original bytes.
func TestRemoveThenRestore(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("dev1", "ideas.md", "the bytes an agent wrote")
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"

	if rec := do(t, h, "POST", base+"remove", map[string]string{"path": "ideas.md"}); rec.Code != 200 {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	// The delete row's restore target is the content it removed — the same
	// predecessor lookup the history view does.
	entries := historyOf(t, h, base, "ideas.md")
	if len(entries) != 2 || entries[1].Blob != shaOf("the bytes an agent wrote") {
		t.Fatalf("history after remove = %+v", entries)
	}
	if rec := do(t, h, "POST", base+"restore", map[string]string{"path": "ideas.md", "sha": entries[1].Blob}); rec.Code != 200 {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", base+"file?path=ideas.md", nil); rec.Body.String() != "the bytes an agent wrote" {
		t.Fatalf("restored content = %q", rec.Body)
	}
}

// The single-volume (plain folder) server has no journal to write a delete
// into, so it has no remove route at all.
func TestRemoveNotOnSingleVolume(t *testing.T) {
	f := newFakeRemote(t)
	f.put("dev1", "f.md", "v1")
	h := f.uploadServer(nil).Handler()
	rec := do(t, h, "POST", "/api/remove", map[string]string{"path": "f.md"})
	if rec.Code < 400 {
		t.Fatalf("single-volume remove: %d, want no such route", rec.Code)
	}
}

func assertJournalsUnchanged(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	after := journalsAt(t, dir)
	if len(after) != len(before) {
		t.Fatalf("a refused remove wrote a journal: %v → %v", before, after)
	}
	for name, data := range before {
		if after[name] != data {
			t.Fatalf("journal %s changed on a refused remove", name)
		}
	}
}
