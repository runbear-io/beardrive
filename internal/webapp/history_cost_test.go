package webapp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// countingDeviceRepo counts how often the registry asks its store whether it
// changed — one round trip each on a database-backed hub.
type countingDeviceRepo struct {
	DeviceRepo
	versions atomic.Int64
}

func (c *countingDeviceRepo) Version() (string, error) {
	c.versions.Add(1)
	return "v1", nil
}

/*
A long feed is attributed per page, per journal — not per matched op.

	Resolving every matched op re-checked the registry's store on each, and on
	a Postgres hub the all-time feed of a busy project took over a minute and
	ended in a 503, so Activity never loaded.
*/
func TestHistoryAttributesThePageNotEveryOp(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	repo := &countingDeviceRepo{DeviceRepo: newFileDeviceRepo(filepath.Join(t.TempDir(), "devices.json"))}
	reg, err := NewDeviceRegistry(repo)
	if err != nil {
		t.Fatal(err)
	}
	srv.Devices = reg
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	for i := 0; i < 400; i++ {
		f.put(fmt.Sprintf("dev%d", i%2), fmt.Sprintf("f%d.md", i), "x")
	}
	repo.versions.Store(0)

	rec := do(t, srv.Handler(), "GET", "/api/p/"+p.ID+"/history?prefix=&n=10", nil)
	var out struct {
		Entries []HistoryEntry `json:"entries"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Entries) != 10 {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	// Two journals on the page: a lookup each, not one per op in the project.
	if n := repo.versions.Load(); n > 2 {
		t.Fatalf("a 10-row page of a 400-op feed checked the device store %d times, want ≤ 2", n)
	}
}

// Emptied is "0 B now and had content before", over the WHOLE journal: a
// file emptied long ago and saved empty many times since is still emptied,
// and a file that was always empty never is.
func TestHistoryEmptied(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("dev1", "notes/daily.md", "standup")
	for i := 0; i < 120; i++ { // more than a history page of empty saves
		f.put("dev1", "notes/daily.md", "")
	}
	f.put("dev1", "robots.txt", "")
	f.put("dev1", "full.md", "content")
	f.put("dev1", "gone.md", "x")
	f.put("dev1", "gone.md", "")
	f.del("dev1", "gone.md")
	// Not emptied: the tree ignores a put with a malformed blob, and a link
	// has no content to have lost.
	f.put("dev1", "bogus.md", "real content")
	f.append("dev1", journal.Op{Kind: journal.KindPut, Path: "bogus.md", Blob: "../../other/blob", Size: 0})
	f.put("dev1", "linked.md", "real content")
	f.append("dev1", journal.Op{Kind: journal.KindPut, Path: "linked.md", Link: "target.md"})
	// Emptied after a move: the content was under its old name.
	t0 := time.Now().Add(-time.Hour)
	f.putAt("dev1", "old-name.md", "moved content", t0)
	f.move("dev1", "old-name.md", "new-name.md", "moved content", t0.Add(time.Minute))
	f.putAt("dev1", "new-name.md", "", t0.Add(2*time.Minute))
	// Not emptied: moved, deleted, then recreated empty — a new file.
	f.putAt("dev1", "src.md", "old content", t0)
	f.move("dev1", "src.md", "dst.md", "old content", t0.Add(time.Minute))
	f.delAt("dev1", "dst.md", t0.Add(2*time.Minute))
	f.putAt("dev1", "dst.md", "", t0.Add(3*time.Minute))

	rec := do(t, srv.Handler(), "GET", "/api/p/"+p.ID+"/history/emptied", nil)
	var out struct{ Paths []string }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("emptied: %d %s", rec.Code, rec.Body)
	}
	if len(out.Paths) != 2 || out.Paths[0] != "new-name.md" || out.Paths[1] != "notes/daily.md" {
		t.Fatalf("emptied = %v, want [new-name.md notes/daily.md]", out.Paths)
	}
}

// A version under a folder the reader cannot see is no evidence of content:
// otherwise a file moved out of a hidden folder and emptied would be listed,
// disclosing that the hidden path held something.
func TestHistoryEmptiedIgnoresHiddenHistory(t *testing.T) {
	h, _, cookies, p, root := permHubAt(t)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	t0 := time.Now().Add(-time.Hour)
	// The content only ever lived under secret/: it was emptied there, then
	// the empty file moved out.
	f.putAt("dev1", "secret/x.md", "hidden content", t0)
	f.putAt("dev1", "secret/x.md", "", t0.Add(time.Minute))
	f.move("dev1", "secret/x.md", "public/x.md", "", t0.Add(2*time.Minute))
	rule := map[string]any{"prefix": "secret", "default": PermNone}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", rule, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("set rule: %d %s", rec.Code, rec.Body)
	}
	get := func(who string) []string {
		t.Helper()
		rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/history/emptied", nil, cookies[who])
		var out struct{ Paths []string }
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("emptied as %s: %d %s", who, rec.Code, rec.Body)
		}
		return out.Paths
	}
	if got := get("bob"); len(got) != 0 {
		t.Fatalf("bob, who cannot see secret/, is told %v was emptied", got)
	}
	if got := get("alice"); len(got) != 1 || got[0] != "public/x.md" {
		t.Fatalf("alice (admin) sees %v, want [public/x.md]", got)
	}
}
