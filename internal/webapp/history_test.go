package webapp

import (
	"crypto/sha256"
	"encoding/hex"
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

// putFull writes a put carrying both an account and a wall-clock time — what
// the filter tests need to slice a feed by author and by day at once.
func (f *fakeRemote) putFull(dev, user, path, content string, at time.Time) {
	f.t.Helper()
	f.putAs(dev, user, strings.ToUpper(user[:1])+user[1:], path, content)
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
	f.putAs("dev1", "alice@x.io", "Alice", "guide.md", "---\nstatus: draft\n---\n\n# Guide\n\nFirst version.\n")
	f.putAs("dev1", "alice@x.io", "Alice", "guide.md", "---\nstatus: final\n---\n\n# Guide\n\nSecond version, longer.\n")
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
		Path        string            `json:"path"`
		HTML        string            `json:"html"`
		Frontmatter []FrontmatterPair `json:"frontmatter"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	// That version's frontmatter, not today's — the panel follows the bytes.
	if len(doc.Frontmatter) != 1 || doc.Frontmatter[0] != (FrontmatterPair{Key: "status", Value: "draft"}) {
		t.Fatalf("version frontmatter = %+v", doc.Frontmatter)
	}
	if strings.Contains(doc.HTML, `class="frontmatter"`) {
		t.Fatalf("version render bakes the table into html: %q", doc.HTML)
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
	if !strings.Contains(rec.Body.String(), "Second version") || !strings.Contains(rec.Body.String(), `"final"`) {
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

// seedFiltered builds a feed with two authors, three days and paths that
// differ in case, so every filter has something to include and something to
// leave out.
func seedFiltered(t *testing.T) (http.Handler, string) {
	t.Helper()
	srv, p, root := newHub(t, false, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	day := func(d, h int) time.Time { return time.Date(2026, 7, d, h, 30, 0, 0, time.UTC) }
	f.putFull("dev1", "mira@acme.io", "docs/Runbook.md", "v1", day(1, 9))
	f.putFull("dev1", "mira@acme.io", "docs/runbook-old.md", "v1", day(15, 12))
	f.putFull("dev2", "ken@acme.io", "docs/plan.md", "p1", day(31, 23))
	f.putFull("dev2", "ken@acme.io", "notes/runbook.md", "n1", day(15, 0))
	return srv.Handler(), "/api/p/" + p.ID + "/"
}

// histPaths runs one history request and returns the paths it yielded, in
// order, plus the next cursor.
func histPaths(t *testing.T, h http.Handler, u string) ([]string, string) {
	t.Helper()
	rec := do(t, h, "GET", u, nil)
	if rec.Code != 200 {
		t.Fatalf("history %s: %d %s", u, rec.Code, rec.Body)
	}
	var out struct {
		Entries []HistoryEntry `json:"entries"`
		Next    string         `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, e := range out.Entries {
		got = append(got, e.Path)
	}
	return got, out.Next
}

// The reader filters: substring, author, date window, and every combination
// of them — including with the existing prefix scoping.
func TestHistoryFilters(t *testing.T) {
	h, base := seedFiltered(t)
	for _, c := range []struct {
		q    string
		want []string
	}{
		// substring, case-insensitive, matching a path the prefix scoping couldn't express
		{"q=runbook", []string{"docs/runbook-old.md", "notes/runbook.md", "docs/Runbook.md"}},
		{"q=RUNBOOK", []string{"docs/runbook-old.md", "notes/runbook.md", "docs/Runbook.md"}},
		{"q=.md", []string{"docs/plan.md", "docs/runbook-old.md", "notes/runbook.md", "docs/Runbook.md"}},
		{"q=nothing-here", []string{}},
		// author, exact
		{"user=mira@acme.io", []string{"docs/runbook-old.md", "docs/Runbook.md"}},
		{"user=ken@acme.io", []string{"docs/plan.md", "notes/runbook.md"}},
		{"user=nobody@acme.io", []string{}},
		// bare dates are UTC days, inclusive at BOTH ends: the 1st 09:30 and
		// the 31st 23:30 both survive a 07-01..07-31 window.
		{"since=2026-07-01&until=2026-07-31", []string{"docs/plan.md", "docs/runbook-old.md", "notes/runbook.md", "docs/Runbook.md"}},
		{"since=2026-07-02", []string{"docs/plan.md", "docs/runbook-old.md", "notes/runbook.md"}},
		{"until=2026-07-15", []string{"docs/runbook-old.md", "notes/runbook.md", "docs/Runbook.md"}},
		{"since=2026-07-15&until=2026-07-15", []string{"docs/runbook-old.md", "notes/runbook.md"}},
		// an RFC3339 bound is inclusive to the second it names
		{"since=2026-07-31T23:30:00Z", []string{"docs/plan.md"}},
		{"until=2026-07-01T09:30:00Z", []string{"docs/Runbook.md"}},
		// since > until means nothing, not an error
		{"since=2026-07-31&until=2026-07-01", []string{}},
		// composed with each other…
		{"q=runbook&user=mira@acme.io", []string{"docs/runbook-old.md", "docs/Runbook.md"}},
		{"q=runbook&user=mira@acme.io&since=2026-07-10&until=2026-07-20", []string{"docs/runbook-old.md"}},
		// …and with the existing prefix/path scoping
		{"prefix=docs/&q=runbook", []string{"docs/runbook-old.md", "docs/Runbook.md"}},
		{"prefix=notes/&user=mira@acme.io", []string{}},
		{"path=docs/Runbook.md&q=runbook", []string{"docs/Runbook.md"}},
		{"path=docs/Runbook.md&user=ken@acme.io", []string{}},
	} {
		if got, _ := histPaths(t, h, base+"history?"+c.q); !slices.Equal(got, c.want) {
			t.Errorf("?%s = %v, want %v", c.q, got, c.want)
		}
	}
}

// Filtering happens before the cursor skip, so paging a filtered feed walks
// exactly the unpaged filtered set — no repeats, no gaps, same order. Filter
// after the skip and this test loses entries.
func TestHistoryFilterPaging(t *testing.T) {
	h, base := seedFiltered(t)
	filter := "q=runbook"
	want, next := histPaths(t, h, base+"history?"+filter)
	if len(want) != 3 || next != "" {
		t.Fatalf("unpaged filtered feed = %v (next %q)", want, next)
	}
	var got []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > len(want) {
			t.Fatalf("paging did not terminate: %v", got)
		}
		page, next := histPaths(t, h, base+"history?"+filter+"&n=2"+cursorArg(cursor))
		if len(page) == 0 {
			t.Fatalf("empty page %d", pages)
		}
		got = append(got, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	if !slices.Equal(got, want) {
		t.Fatalf("paged filtered = %v, want %v", got, want)
	}
}

// A date we can't parse is a 400, not a silently unfiltered feed.
func TestHistoryBadDateRange(t *testing.T) {
	h, base := seedFiltered(t)
	for _, q := range []string{"since=yesterday", "until=2026-13-45", "since=2026-07-01&until=soon", "since="} {
		rec := do(t, h, "GET", base+"history?"+q, nil)
		if q == "since=" { // an empty value is "no filter", like every other param
			if rec.Code != 200 {
				t.Errorf("?%s = %d, want 200", q, rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("?%s = %d, want 400", q, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "\"entries\"") {
			t.Errorf("?%s returned a feed body: %s", q, rec.Body)
		}
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

// setSession stamps the last op in dev's journal with an agent session, as
// the agent sync hook does.
func (f *fakeRemote) setSession(dev, session string) {
	f.t.Helper()
	p := filepath.Join(f.dir, "journal", dev+".jsonl")
	ops, err := journal.ReadFile(p)
	if err != nil || len(ops) == 0 {
		f.t.Fatal(err)
	}
	ops[len(ops)-1].Session = session
	data, err := journal.Marshal(ops)
	if err != nil {
		f.t.Fatal(err)
	}
	writeFileT(f.t, p, data)
}

// ?actor= splits on the hook-set session, never the note; ?device= on the
// journal the op came from.
func TestHistoryActorAndDeviceFilters(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	day := func(d int) time.Time { return time.Date(2026, 7, d, 12, 0, 0, 0, time.UTC) }
	f.putFull("dev1", "mira@acme.io", "a.md", "a", day(1))
	f.setSession("dev1", "s1")
	f.putFull("dev1", "mira@acme.io", "b.md", "b", day(2))
	f.putFull("dev2", "ken@acme.io", "c.md", "c", day(3))
	h, base := srv.Handler(), "/api/p/"+p.ID+"/"
	for _, c := range []struct {
		q    string
		want []string
	}{
		{"actor=agent", []string{"a.md"}},
		{"actor=human", []string{"c.md", "b.md"}},
		{"device=dev1", []string{"b.md", "a.md"}},
		{"device=dev2&actor=agent", []string{}},
		{"device=nope", []string{}},
	} {
		if got, _ := histPaths(t, h, base+"history?"+c.q); !slices.Equal(got, c.want) {
			t.Errorf("?%s = %v, want %v", c.q, got, c.want)
		}
	}
	if rec := do(t, h, "GET", base+"history?actor=robot", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("?actor=robot = %d, want 400", rec.Code)
	}
}

// The summary counts the last N calendar days in the reader's zone, split
// agent/human, and honours the same filters as the feed.
func TestHistorySummary(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	loc := time.FixedZone("", 9*3600)
	now := time.Now().In(loc)
	y, m, d := now.Date()
	at := func(daysAgo, hour int) time.Time {
		return time.Date(y, m, d-daysAgo, hour, 0, 0, 0, loc)
	}
	// 00:30 local today is still yesterday in UTC: the zone decides the bar.
	f.putFull("dev1", "mira@acme.io", "a.md", "a", time.Date(y, m, d, 0, 30, 0, 0, loc))
	f.setSession("dev1", "s1")
	f.putFull("dev1", "mira@acme.io", "b.md", "b", at(1, 10))
	f.putFull("dev2", "ken@acme.io", "docs/c.md", "c", at(6, 10))
	f.putFull("dev2", "ken@acme.io", "docs/old.md", "o", at(7, 10)) // outside 7 days
	h, base := srv.Handler(), "/api/p/"+p.ID+"/"

	type sum struct {
		Total, Agent, Human, Devices int
		Days                         []histDay
	}
	get := func(q string) sum {
		t.Helper()
		rec := do(t, h, "GET", base+"history/summary?"+q, nil)
		if rec.Code != 200 {
			t.Fatalf("summary ?%s: %d %s", q, rec.Code, rec.Body)
		}
		var s sum
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := get("offset=540")
	if s.Total != 3 || s.Agent != 1 || s.Human != 2 || s.Devices != 2 || len(s.Days) != 7 {
		t.Fatalf("summary = %+v", s)
	}
	if last := s.Days[6]; last.Date != now.Format("2006-01-02") || last.Agent != 1 || last.Human != 0 {
		t.Errorf("today = %+v", last)
	}
	if s.Days[0].Human != 1 {
		t.Errorf("six days ago = %+v", s.Days[0])
	}
	if s := get("offset=540&days=8"); s.Total != 4 || len(s.Days) != 8 {
		t.Errorf("days=8 = %+v", s)
	}
	if s := get("offset=540&prefix=docs/"); s.Total != 1 {
		t.Errorf("prefix = %+v", s)
	}
	if s := get("offset=540&actor=agent"); s.Total != 1 || s.Human != 0 {
		t.Errorf("actor = %+v", s)
	}
	// A custom range charts the days its bounds touch, in the reader's zone,
	// and counts only what the feed under the same bounds would show.
	bound := func(t time.Time) string { return url.QueryEscape(t.Format(time.RFC3339)) }
	since, until := at(7, 0), at(6, 0).Add(24*time.Hour-time.Second)
	s = get("offset=540&since=" + bound(since) + "&until=" + bound(until))
	if s.Total != 2 || len(s.Days) != 2 || s.Days[0].Date != since.Format("2006-01-02") ||
		s.Days[1].Date != at(6, 0).Format("2006-01-02") || s.Days[0].Human != 1 || s.Days[1].Human != 1 {
		t.Errorf("custom range = %+v", s)
	}
	if s := get("offset=540&until=" + bound(until)); s.Total != 2 || len(s.Days) != 7 || s.Days[6].Date != at(6, 0).Format("2006-01-02") {
		t.Errorf("until only = %+v", s)
	}
	if s := get("offset=540&since=" + bound(at(1, 0))); s.Total != 2 || len(s.Days) != 2 {
		t.Errorf("since only = %+v", s)
	}
	if s := get("offset=540&since=" + bound(at(90, 0))); len(s.Days) != 31 || s.Days[30].Date != now.Format("2006-01-02") {
		t.Errorf("long range keeps the latest 31 days: %d days", len(s.Days))
	}
	if s := get("offset=540&since=" + bound(at(1, 0)) + "&until=" + bound(at(3, 0))); s.Total != 0 || len(s.Days) != 0 {
		t.Errorf("since after until = %+v", s)
	}
	for _, q := range []string{"days=0", "days=32", "actor=x"} {
		if rec := do(t, h, "GET", base+"history/summary?"+q, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("?%s = %d, want 400", q, rec.Code)
		}
	}
}

// Every entry names the content its path had just before it, from the whole
// journal rather than the page, so a client can diff a change whose
// predecessor it never loaded and tell that from a genuine first version.
func TestHistoryEntryPrev(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	sha := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	f.put("dev1", "a.md", "v1")
	f.put("dev1", "a.md", "v2")
	f.del("dev1", "a.md")
	f.del("dev2", "a.md") // a second device deleting the same, already-gone file
	f.put("dev1", "a.md", "v3")
	h, base := srv.Handler(), "/api/p/"+p.ID+"/"

	got := func(q string) []HistoryEntry {
		t.Helper()
		rec := do(t, h, "GET", base+"history?"+q, nil)
		var out struct {
			Entries []HistoryEntry `json:"entries"`
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("history ?%s: %d %s", q, rec.Code, rec.Body)
		}
		return out.Entries
	}
	all := got("path=a.md")
	want := []struct{ kind, prev string }{
		{"add", sha("v2")},    // re-added: replaces what the delete removed
		{"delete", ""},        // the path was already gone: removed nothing
		{"delete", sha("v2")}, // names what it removed
		{"edit", sha("v1")},
		{"add", ""}, // a genuine first version
	}
	if len(all) != len(want) {
		t.Fatalf("entries = %+v", all)
	}
	for i, w := range want {
		if all[i].Kind != w.kind || all[i].Prev != w.prev {
			t.Errorf("entry %d = %s prev %q, want %s prev %q", i, all[i].Kind, all[i].Prev, w.kind, w.prev)
		}
	}
	// A one-entry page still knows the delete's predecessor.
	if page := got("path=a.md&n=1"); len(page) != 1 || page[0].Prev != sha("v2") {
		t.Errorf("first page = %+v", page)
	}
}
