package webapp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

/*
A synced symlink on the hub: listed as a link, never served as content.

	A device journals a symlink as its target string and no blob
	(syncer/symlink_test.go). The hub folds that op into the tree as a link
	entry — name, target, no size — and every content door refuses it, since
	there are no bytes behind it on the hub and following the target would
	mean resolving a path a peer chose. The dir-mode half of this contract is
	TestSec_Path_DirSymlinkIsNotServed.
*/
func TestHubListsASyncedSymlinkAndNeverServesIt(t *testing.T) {
	h, _, c, p := permHub(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	link := func(seq int64, path, target string) map[string]any {
		return map[string]any{
			"seq": seq, "lamport": seq, "time": now,
			"device": "alice-desk", "device_name": "Alice Desk",
			"kind": "put", "path": path, "link": target,
		}
	}
	sec6PushJournal(t, h, p.ID, "alice-desk", []map[string]any{
		link(1, "docs/alias.md", "real.md"),
		link(2, "escape.txt", "/etc/passwd"),
	}, c["alice"])

	var tree struct {
		Children []*struct {
			Name     string `json:"name"`
			Path     string `json:"path"`
			Link     string `json:"link"`
			Size     int64  `json:"size"`
			Children []*struct {
				Path string `json:"path"`
				Link string `json:"link"`
				Size int64  `json:"size"`
			} `json:"children"`
		} `json:"children"`
	}
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["alice"])
	if err := json.Unmarshal(rec.Body.Bytes(), &tree); err != nil {
		t.Fatalf("tree: %d %s", rec.Code, rec.Body)
	}
	seen := map[string]string{}
	for _, n := range tree.Children {
		if n.Link != "" && n.Size == 0 {
			seen[n.Path] = n.Link
		}
		for _, k := range n.Children {
			if k.Link != "" && k.Size == 0 {
				seen[k.Path] = k.Link
			}
		}
	}
	if seen["docs/alias.md"] != "real.md" || seen["escape.txt"] != "/etc/passwd" {
		t.Fatalf("tree does not list the links as links: %v in %s", seen, rec.Body)
	}

	for _, route := range []string{"file", "download", "render"} {
		for _, path := range []string{"docs/alias.md", "escape.txt"} {
			rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/"+route+"?path="+path, nil, c["alice"])
			if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "root:") {
				t.Errorf("/api/p/<id>/%s?path=%s served a symlink (%d): %.120s", route, path, rec.Code, rec.Body)
			}
		}
	}
}
