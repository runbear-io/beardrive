package webapp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// seed writes a put op carrying a signed-in account, like a logged-in device
// does.
func (f *fakeRemote) putAs(dev, user, userName, path, content string) {
	f.t.Helper()
	f.put(dev, path, content)
	// rewrite the last op to carry the account (put() doesn't know it)
	p := filepath.Join(f.dir, "journal", dev+".jsonl")
	ops, err := journal.ReadFile(p)
	if err != nil || len(ops) == 0 {
		f.t.Fatal(err)
	}
	ops[len(ops)-1].User, ops[len(ops)-1].UserName = user, userName
	data, err := journal.Marshal(ops)
	if err != nil {
		f.t.Fatal(err)
	}
	writeFileT(f.t, p, data)
}

// putAt writes a put whose wall-clock time is `at`. The Lamport clock still
// advances in call order, so a test can make causal and chronological order
// disagree — exactly what an offline device produces.
func (f *fakeRemote) putAt(dev, path, content string, at time.Time) {
	f.t.Helper()
	f.put(dev, path, content)
	p := filepath.Join(f.dir, "journal", dev+".jsonl")
	ops, err := journal.ReadFile(p)
	if err != nil || len(ops) == 0 {
		f.t.Fatal(err)
	}
	ops[len(ops)-1].Time = at
	data, err := journal.Marshal(ops)
	if err != nil {
		f.t.Fatal(err)
	}
	writeFileT(f.t, p, data)
}

func writeFileT(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryAPI(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.putAs("dev1", "alice@x.io", "Alice", "notes/plan.md", "v1")
	f.putAs("dev1", "alice@x.io", "Alice", "notes/plan.md", "v2 longer")
	f.putAs("dev2", "bob@x.io", "Bob", "notes/other.md", "bob's file")
	f.del("dev2", "notes/other.md")
	f.putAs("dev1", "alice@x.io", "Alice", "readme.md", "top")

	// the server knows dev1 from its store traffic
	srv.Devices, _ = OpenDeviceRegistry(filepath.Join(t.TempDir(), "devices.json"))
	srv.Devices.Observe(DeviceInfo{ID: "dev1", Name: "alice-laptop", OS: "darwin/arm64", IP: "203.0.113.7"})

	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"

	// one file's versions, newest first
	rec := do(t, h, "GET", base+"history?path=notes/plan.md", nil)
	if rec.Code != 200 {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Entries []HistoryEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(out.Entries))
	}
	newest, oldest := out.Entries[0], out.Entries[1]
	if newest.Size != int64(len("v2 longer")) || oldest.Size != int64(len("v1")) {
		t.Fatalf("order wrong: %+v", out.Entries)
	}
	// puts are classified: first version = add, later versions = edit
	if oldest.Kind != "add" || newest.Kind != "edit" {
		t.Fatalf("kinds = %q, %q; want add, edit", oldest.Kind, newest.Kind)
	}
	if newest.User != "alice@x.io" || newest.UserName != "Alice" {
		t.Fatalf("user = %+v", newest)
	}
	// device joined from the registry: name and OS only — the server-observed
	// IP stays in the registry and out of every member's history feed (BEA-43).
	// The id always rides along, so a nameless device still renders as something.
	if newest.Device.ID != "dev1" || newest.Device.Name != "alice-laptop" || newest.Device.OS != "darwin/arm64" {
		t.Fatalf("device = %+v", newest.Device)
	}
	// asserted on the raw body, not the struct: a typed unmarshal would pass
	// even if the server still emitted the key.
	if strings.Contains(rec.Body.String(), "203.0.113.7") {
		t.Fatalf("history response leaks the device IP: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "last_seen") {
		t.Fatalf("history response carries registry internals: %s", rec.Body)
	}
	if d, ok := srv.Devices.Get("dev1"); !ok || d.IP != "203.0.113.7" {
		t.Fatalf("registry must keep observing the IP: %+v %v", d, ok)
	}
	if newest.Blob == "" || oldest.Blob == "" {
		t.Fatal("entries must link to their exact content")
	}

	// folder rollup: everything under notes/, deletes included
	rec = do(t, h, "GET", base+"history?prefix=notes/", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 4 {
		t.Fatalf("notes/ feed = %d entries, want 4", len(out.Entries))
	}
	if out.Entries[0].Kind != "delete" || out.Entries[0].Path != "notes/other.md" {
		t.Fatalf("newest notes/ entry = %+v, want the delete", out.Entries[0])
	}
	// the put that created other.md is an add, even in the filtered view
	if out.Entries[1].Kind != "add" || out.Entries[1].Path != "notes/other.md" {
		t.Fatalf("entry before the delete = %+v, want other.md's add", out.Entries[1])
	}
	// a device the registry never saw falls back to the op's own info
	if out.Entries[0].Device.ID != "dev2" || out.Entries[0].Device.Name != "dev2" {
		t.Fatalf("unknown device fallback = %+v", out.Entries[0].Device)
	}
	// the prefix feed is the same projection — no IP there either
	if strings.Contains(rec.Body.String(), "203.0.113.7") {
		t.Fatalf("prefix feed leaks the device IP: %s", rec.Body)
	}

	// whole-project feed + n limit
	rec = do(t, h, "GET", base+"history?n=2", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 2 {
		t.Fatalf("n=2 gave %d entries", len(out.Entries))
	}

	// any old version is retrievable by content hash
	rec = do(t, h, "GET", base+"blob?sha="+oldest.Blob+"&name=plan.md", nil)
	if rec.Code != 200 || rec.Body.String() != "v1" {
		t.Fatalf("old version: %d %q", rec.Code, rec.Body)
	}
	rec = do(t, h, "GET", base+"blob?sha="+oldest.Blob+"&name=plan.md&download=1", nil)
	if cd := rec.Header().Get("Content-Disposition"); cd == "" {
		t.Fatal("download variant should attach")
	}
	if rec := do(t, h, "GET", base+"blob?sha=nothex", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad sha: %d, want 400", rec.Code)
	}
}

// An old markdown version renders as markdown, not raw source — and looking
// at history is never a read.
func TestRenderVersion(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.putAs("dev1", "alice@x.io", "Alice", "guide.md", "# Guide\n\nFirst version.\n")
	f.putAs("dev1", "alice@x.io", "Alice", "guide.md", "# Guide\n\nSecond version, longer.\n")
	var err error
	if srv.Reads, err = OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"

	rec := do(t, h, "GET", base+"history?path=guide.md", nil)
	var hist struct {
		Entries []HistoryEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	first := hist.Entries[len(hist.Entries)-1] // oldest

	rec = do(t, h, "GET", base+"render?path=guide.md&sha="+first.Blob, nil)
	if rec.Code != 200 {
		t.Fatalf("render version: %d %s", rec.Code, rec.Body)
	}
	var doc struct {
		Path string `json:"path"`
		HTML string `json:"html"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.HTML, "First version") || strings.Contains(doc.HTML, "Second version") {
		t.Fatalf("rendered the wrong version: %q", doc.HTML)
	}
	if !strings.Contains(doc.HTML, "<h1") { // rendered, not raw source
		t.Fatalf("not markdown-rendered: %q", doc.HTML)
	}
	if doc.Path != "guide.md" {
		t.Fatalf("path = %q", doc.Path)
	}
	// current content still renders from the snapshot
	rec = do(t, h, "GET", base+"render?path=guide.md", nil)
	if !strings.Contains(rec.Body.String(), "Second version") {
		t.Fatalf("current render = %s", rec.Body)
	}

	if rec := do(t, h, "GET", base+"render?path=guide.md&sha=nothex", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad sha: %d, want 400", rec.Code)
	}
	missing := strings.Repeat("a", 64)
	if rec := do(t, h, "GET", base+"render?path=guide.md&sha="+missing, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown sha: %d, want 404", rec.Code)
	}

	// Only the current-content render above counted; the version views did not.
	rec = do(t, h, "GET", base+"heat", nil)
	var heat struct {
		Entries map[string]HeatEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &heat); err != nil {
		t.Fatal(err)
	}
	if e := heat.Entries["guide.md"]; e.Human != 1 {
		t.Fatalf("heat = %+v; viewing history must not count as a read", heat.Entries)
	}
}

// History is newest-first by wall-clock time, not by Lamport clock: a device
// that was offline writes later in real time but carries a lower clock, so
// reverse-journal order would bury the most recent change.
func TestHistoryOrderedByTimeNotLamport(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	early := time.Date(2026, 7, 26, 0, 9, 17, 0, time.UTC)
	late := time.Date(2026, 7, 26, 22, 9, 17, 0, time.UTC)
	// lamport 1 but newest by the clock — the offline device
	f.putAt("offline", "notes/late.md", "written offline", late)
	// lamport 2..4, all at the same earlier timestamp
	f.putAt("online", "notes/a.md", "a", early)
	f.putAt("online", "notes/b.md", "b", early)
	f.putAt("online", "notes/c.md", "c", early)

	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"
	paths := func(url string) []string {
		t.Helper()
		rec := do(t, h, "GET", url, nil)
		if rec.Code != 200 {
			t.Fatalf("history: %d %s", rec.Code, rec.Body)
		}
		var out struct {
			Entries []HistoryEntry `json:"entries"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range out.Entries {
			if e.Kind != "add" { // first version of each path, whatever the display order
				t.Fatalf("kind = %q for %s, want add", e.Kind, e.Path)
			}
			got = append(got, e.Path+"@"+e.Time)
		}
		return got
	}

	// newest by time first, then the equal-timestamp rows in descending Lamport
	want := []string{
		"notes/late.md@2026-07-26T22:09:17Z",
		"notes/c.md@2026-07-26T00:09:17Z",
		"notes/b.md@2026-07-26T00:09:17Z",
		"notes/a.md@2026-07-26T00:09:17Z",
	}
	got := paths(base + "history")
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	// ?n= selects the n most recent BY TIME, not the n highest-Lamport
	if got := paths(base + "history?n=1"); !slices.Equal(got, want[:1]) {
		t.Fatalf("n=1 = %v, want %v", got, want[:1])
	}
	// equal timestamps come back in a stable order across requests
	if again := paths(base + "history"); !slices.Equal(again, want) {
		t.Fatalf("repeat = %v, want %v", again, want)
	}
	// a filtered view sorts the same way
	if got := paths(base + "history?prefix=notes/"); !slices.Equal(got, want) {
		t.Fatalf("prefix order = %v, want %v", got, want)
	}
}

// Paging walks the same order the feed displays: each entry exactly once, in
// time order, across boundaries that fall between ops whose Lamport order and
// wall-clock order disagree — and between two ops sharing one timestamp,
// which the whole-second `time` field could not have expressed, so only a
// server-minted cursor can resume there.
func TestHistoryPagingAcrossLamportAndTime(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	early := time.Date(2026, 7, 26, 0, 9, 17, 0, time.UTC)
	late := time.Date(2026, 7, 26, 22, 9, 17, 0, time.UTC)
	f.putAt("offline", "notes/late.md", "written offline", late) // lamport 1, newest by the clock
	f.putAt("online", "notes/a.md", "a", early)                  // lamport 2..4, one shared timestamp
	f.putAt("online", "notes/b.md", "b", early)
	f.putAt("online", "notes/c.md", "c", early)

	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"
	// page returns one response's paths (path@time) and its next cursor.
	page := func(u string) ([]string, string) {
		t.Helper()
		rec := do(t, h, "GET", u, nil)
		if rec.Code != 200 {
			t.Fatalf("history: %d %s", rec.Code, rec.Body)
		}
		var out struct {
			Entries []HistoryEntry `json:"entries"`
			Next    string         `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range out.Entries {
			got = append(got, e.Path+"@"+e.Time)
		}
		return got, out.Next
	}

	want := []string{
		"notes/late.md@2026-07-26T22:09:17Z",
		"notes/c.md@2026-07-26T00:09:17Z",
		"notes/b.md@2026-07-26T00:09:17Z",
		"notes/a.md@2026-07-26T00:09:17Z",
	}
	// one entry at a time, following the cursor to the end
	var got []string
	cursor, pages := "", 0
	for {
		entries, next := page(base + "history?n=1" + cursorArg(cursor))
		if len(entries) != 1 {
			t.Fatalf("page %d = %v, want exactly 1 entry", pages, entries)
		}
		got = append(got, entries...)
		pages++
		if pages > len(want)+2 {
			t.Fatalf("paging did not terminate: %v", got)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if !slices.Equal(got, want) {
		t.Fatalf("paged = %v, want %v (each entry once, in time order)", got, want)
	}
	if pages != len(want) {
		t.Fatalf("pages = %d, want %d", pages, len(want))
	}

	// mid-list cursors: page 2 of 2 picks up exactly where page 1 stopped
	first, next := page(base + "history?n=2")
	if !slices.Equal(first, want[:2]) || next == "" {
		t.Fatalf("page 1 = %v (next %q)", first, next)
	}
	second, next := page(base + "history?n=2&cursor=" + url.QueryEscape(next))
	if !slices.Equal(second, want[2:]) {
		t.Fatalf("page 2 = %v, want %v", second, want[2:])
	}
	if next != "" {
		t.Fatalf("last page carries next_cursor %q", next)
	}

	// a request that fits in one page never claims there is more
	if all, next := page(base + "history?n=100"); !slices.Equal(all, want) || next != "" {
		t.Fatalf("single page = %v (next %q)", all, next)
	}
	// the prefix feed pages the same way
	if got, _ := page(base + "history?prefix=notes/&n=2"); !slices.Equal(got, want[:2]) {
		t.Fatalf("prefix page 1 = %v", got)
	}
	// a garbage cursor is an error, not a silent full page
	if rec := do(t, h, "GET", base+"history?cursor=not-a-cursor", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad cursor: %d, want 400", rec.Code)
	}
}

// BenchmarkHistoryPage measures what a deep page costs: every request
// re-lists and re-parses every journal (loadOps), so page 20 should cost
// about what page 1 costs — the ceiling is gone, the per-page work is not.
func BenchmarkHistoryPage(b *testing.B) {
	srv, p, root := newHub(b, false, nil)
	dir := filepath.Join(root, p.ID)
	os.MkdirAll(filepath.Join(dir, "journal"), 0o755)
	os.MkdirAll(filepath.Join(dir, "blobs"), 0o755)
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	ops := make([]journal.Op, 0, 5000)
	for i := range 5000 {
		ops = append(ops, journal.Op{
			Seq: int64(i + 1), Lamport: int64(i + 1),
			Time: now.Add(-time.Duration(i) * time.Minute), Device: "bench",
			Kind: journal.KindPut, Path: fmt.Sprintf("docs/%03d.md", i%50),
			Blob: strings.Repeat("a", 64), Size: 12, Mode: 0o644,
		})
	}
	if err := journal.Append(filepath.Join(dir, "journal", "bench.jsonl"), ops); err != nil {
		b.Fatal(err)
	}
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/history?n=100"
	get := func(u string) string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", u, nil))
		if rec.Code != 200 {
			b.Fatalf("history: %d %s", rec.Code, rec.Body)
		}
		var out struct {
			Next string `json:"next_cursor"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Next
	}
	// the cursor that opens page 20, paid for once outside the timed loop
	deep := ""
	for range 19 {
		deep = get(base + cursorArg(deep))
	}
	b.Run("page1", func(b *testing.B) {
		for b.Loop() {
			get(base)
		}
	})
	b.Run("page20", func(b *testing.B) {
		for b.Loop() {
			get(base + cursorArg(deep))
		}
	})
}

func cursorArg(c string) string {
	if c == "" {
		return ""
	}
	return "&cursor=" + url.QueryEscape(c)
}

func TestDeviceRegistryObserve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	r, err := OpenDeviceRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	r.Observe(DeviceInfo{ID: "d1", Name: "laptop", OS: "darwin/arm64", User: "a@x.io", IP: "198.51.100.4"})
	// identity survives a restart
	r2, err := OpenDeviceRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := r2.Get("d1")
	if !ok || d.Name != "laptop" || d.IP != "198.51.100.4" || d.User != "a@x.io" {
		t.Fatalf("reloaded = %+v %v", d, ok)
	}
	if time.Since(d.LastSeen) > time.Minute {
		t.Fatalf("last_seen = %v", d.LastSeen)
	}
	// partial updates don't erase known fields
	r2.Observe(DeviceInfo{ID: "d1", IP: "198.51.100.9"})
	d, _ = r2.Get("d1")
	if d.Name != "laptop" || d.IP != "198.51.100.9" {
		t.Fatalf("merge = %+v", d)
	}
	// nil registry is a no-op
	var nilReg *DeviceRegistry
	nilReg.Observe(DeviceInfo{ID: "x"})
	if _, ok := nilReg.Get("x"); ok {
		t.Fatal("nil registry returned a device")
	}
}

// The store API records what it sees about devices (headers + observed IP +
// authenticated user).
func TestStoreObservesDevices(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	srv.Devices, _ = OpenDeviceRegistry(filepath.Join(t.TempDir(), "devices.json"))
	h := srv.Handler()

	// Its own journal push: a read grants nothing, so it claims nothing.
	req := httptest.NewRequest("PUT", "/api/p/"+p.ID+"/store/object?key=journal/dev-9.jsonl", nil)
	req.Header.Set("X-Bdrive-Device", "dev-9")
	req.Header.Set("X-Bdrive-Device-Name", "build-box")
	req.Header.Set("X-Bdrive-Os", "linux/amd64")
	req.RemoteAddr = "192.0.2.55:41000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
	d, ok := srv.Devices.Get("dev-9")
	if !ok || d.Name != "build-box" || d.OS != "linux/amd64" || d.IP != "192.0.2.55" {
		t.Fatalf("observed = %+v %v", d, ok)
	}
}
