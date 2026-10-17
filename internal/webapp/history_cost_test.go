package webapp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
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

	rec := do(t, srv.Handler(), "GET", "/api/p/"+p.ID+"/history/emptied", nil)
	var out struct{ Paths []string }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("emptied: %d %s", rec.Code, rec.Body)
	}
	if len(out.Paths) != 1 || out.Paths[0] != "notes/daily.md" {
		t.Fatalf("emptied = %v, want [notes/daily.md]", out.Paths)
	}
}
