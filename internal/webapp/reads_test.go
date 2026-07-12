package webapp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// doHdr is do() with request headers (device identity on read reports).
func doHdr(t *testing.T, h http.Handler, method, url string, body any, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, url, bytes.NewReader(data))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func openTestLedger(t *testing.T, retentionDays int) (*ReadLedger, *fileReadRepo) {
	t.Helper()
	repo := newFileReadRepo(filepath.Join(t.TempDir(), "reads.json"))
	l, err := NewReadLedger(repo, retentionDays)
	if err != nil {
		t.Fatal(err)
	}
	return l, repo
}

func TestReadLedgerDebounce(t *testing.T) {
	l, _ := openTestLedger(t, 0)
	// A reload storm and the render-then-raw double fetch are one visit…
	l.Record("p-1", "a.md", ReadKindHuman, "alice@x.io")
	l.Record("p-1", "a.md", ReadKindHuman, "alice@x.io")
	l.Record("p-1", "a.md", ReadKindHuman, "alice@x.io")
	// …but a different actor, kind, or path counts on its own.
	l.Record("p-1", "a.md", ReadKindHuman, "bob@x.io")
	l.Record("p-1", "a.md", ReadKindAgent, "alice@x.io")
	l.Record("p-1", "b.md", ReadKindHuman, "alice@x.io")
	heat := l.Heat("p-1", "", time.Time{})
	if e := heat["a.md"]; e.Human != 2 || e.Agent != 1 || e.Readers != 2 {
		t.Fatalf("a.md = %+v, want human 2, agent 1, readers 2", e)
	}
	if e := heat["b.md"]; e.Human != 1 || e.Readers != 1 {
		t.Fatalf("b.md = %+v", e)
	}
	if heat["a.md"].LastRead.IsZero() {
		t.Fatal("last_read not set")
	}
}

func TestReadLedgerWindow(t *testing.T) {
	l, repo := openTestLedger(t, 0)
	l.Record("p-1", "a.md", ReadKindHuman, "alice@x.io")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	// An old bucket inside retention: counted all-time, outside a 7-day window.
	old := ReadStat{Project: "p-1", Path: "a.md", Day: "2026-01-01", Kind: ReadKindHuman,
		Actor: "carol@x.io", Count: 5, Last: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	if err := repo.PutBatch([]ReadStat{old}); err != nil {
		t.Fatal(err)
	}
	l2, err := NewReadLedger(repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	if e := l2.Heat("p-1", "", time.Time{})["a.md"]; e.Human != 6 || e.Readers != 2 {
		t.Fatalf("all-time = %+v, want human 6, readers 2", e)
	}
	week := time.Now().UTC().AddDate(0, 0, -7)
	if e := l2.Heat("p-1", "", week)["a.md"]; e.Human != 1 || e.Readers != 1 {
		t.Fatalf("windowed = %+v, want only today's read", e)
	}
}

func TestReadLedgerRetentionFold(t *testing.T) {
	repo := newFileReadRepo(filepath.Join(t.TempDir(), "reads.json"))
	seed := []ReadStat{
		{Project: "p-1", Path: "a.md", Day: "2020-01-01", Kind: ReadKindHuman, Actor: "alice@x.io", Count: 3,
			Last: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
		{Project: "p-1", Path: "a.md", Day: "2020-01-02", Kind: ReadKindHuman, Actor: "alice@x.io", Count: 2,
			Last: time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)},
		{Project: "p-1", Path: "a.md", Day: time.Now().UTC().Format("2006-01-02"), Kind: ReadKindHuman,
			Actor: "alice@x.io", Count: 1, Last: time.Now().UTC()},
	}
	if err := repo.PutBatch(seed); err != nil {
		t.Fatal(err)
	}
	l, err := NewReadLedger(repo, 30)
	if err != nil {
		t.Fatal(err)
	}
	// All-time totals survive the fold; per-day resolution ages out.
	if e := l.Heat("p-1", "", time.Time{})["a.md"]; e.Human != 6 || e.Readers != 1 {
		t.Fatalf("after fold = %+v, want human 6, readers 1", e)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.Load()
	if err != nil {
		t.Fatal(err)
	}
	var folds, dailies int
	for _, st := range rows {
		if st.Day == "" {
			folds++
			if st.Count != 5 {
				t.Fatalf("fold count = %d, want 5", st.Count)
			}
		} else {
			dailies++
		}
	}
	if folds != 1 || dailies != 1 {
		t.Fatalf("rows after fold: %d folds, %d dailies; want 1 and 1 (%+v)", folds, dailies, rows)
	}
	// Reloading must not double-count: the fold replaced the old rows.
	l2, err := NewReadLedger(repo, 30)
	if err != nil {
		t.Fatal(err)
	}
	if e := l2.Heat("p-1", "", time.Time{})["a.md"]; e.Human != 6 {
		t.Fatalf("after reload = %+v, want human still 6", e)
	}
}

func TestReadLedgerNil(t *testing.T) {
	var l *ReadLedger
	l.Record("p-1", "a.md", ReadKindHuman, "x") // must not panic
	if l.Heat("p-1", "", time.Time{}) != nil {
		t.Fatal("nil ledger heat should be nil")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestHeatAPI drives the recording sites and the heat/report endpoints
// through the real handler: renders and downloads count (debounced), the
// store sync proxy and history blob views never count, and device-reported
// agent reads land as agent traffic.
func TestHeatAPI(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.putAs("dev1", "alice@x.io", "Alice", "wiki/plan.md", "# plan")
	f.putAs("dev1", "alice@x.io", "Alice", "top.md", "top")
	var err error
	srv.Reads, err = OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"

	// render + raw fetch of the same file = one visit (debounced)
	for _, u := range []string{base + "render?path=wiki/plan.md", base + "file?path=wiki/plan.md"} {
		if rec := do(t, h, "GET", u, nil); rec.Code != 200 {
			t.Fatalf("GET %s: %d %s", u, rec.Code, rec.Body)
		}
	}
	// store proxy traffic is replication, not reading
	do(t, h, "GET", base+"store/list?prefix=journal/", nil)
	do(t, h, "GET", base+"store/object?key=journal/dev1.jsonl", nil)

	rec := do(t, h, "GET", base+"heat", nil)
	if rec.Code != 200 {
		t.Fatalf("heat: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Entries map[string]HeatEntry `json:"entries"`
		Since   string               `json:"since"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if e := out.Entries["wiki/plan.md"]; e.Human != 1 || e.Readers != 1 {
		t.Fatalf("plan.md heat = %+v, want one human visit", e)
	}
	if len(out.Entries) != 1 || out.Since == "" {
		t.Fatalf("heat = %+v; store traffic must not count", out)
	}

	// an agent device reports its local tool reads
	report := map[string]any{
		"reads": []map[string]string{{"path": "wiki/plan.md"}, {"path": "top.md"}, {"path": "../evil"}},
	}
	rec = doHdr(t, h, "POST", base+"reads", report, map[string]string{"X-Bdrive-Device": "dev1"})
	if rec.Code != 200 {
		t.Fatalf("report: %d %s", rec.Code, rec.Body)
	}
	var acc struct {
		Accepted int `json:"accepted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	if acc.Accepted != 2 {
		t.Fatalf("accepted = %d, want 2 (traversal path dropped)", acc.Accepted)
	}
	// …and without a device identity the report is rejected
	if rec := doHdr(t, h, "POST", base+"reads", report, nil); rec.Code != 400 {
		t.Fatalf("device-less report: %d, want 400", rec.Code)
	}

	rec = do(t, h, "GET", base+"heat?prefix=wiki", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if e := out.Entries["wiki/plan.md"]; e.Human != 1 || e.Agent != 1 {
		t.Fatalf("plan.md after report = %+v, want human 1 + agent 1", e)
	}
	if _, ok := out.Entries["top.md"]; ok {
		t.Fatal("prefix filter leaked top.md")
	}

	if rec := do(t, h, "GET", base+"heat?days=x", nil); rec.Code != 400 {
		t.Fatalf("bad days: %d, want 400", rec.Code)
	}

	// a hub without read tracking 404s cleanly
	srv.Reads = nil
	if rec := do(t, h, "GET", base+"heat", nil); rec.Code != 404 {
		t.Fatalf("disabled heat: %d, want 404", rec.Code)
	}
}

// TestHeatByDevice covers the coverage-matrix breakdown: agent reads per
// device per top-level folder, device-registry joined — and, critically,
// that human actor identities never appear in the response.
func TestHeatByDevice(t *testing.T) {
	srv, p, root := newHub(t, false, nil)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.putAs("dev1", "alice@x.io", "Alice", "wiki/plan.md", "# plan")
	var err error
	srv.Reads, err = OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	srv.Devices, _ = OpenDeviceRegistry(filepath.Join(t.TempDir(), "devices.json"))
	srv.Devices.Observe(DeviceInfo{ID: "dev1", Name: "ci-agent", OS: "linux/amd64"})

	// Agent reads from two devices, human reads carrying real emails.
	srv.Reads.Record(p.ID, "wiki/plan.md", ReadKindAgent, "dev1")
	srv.Reads.Record(p.ID, "wiki/deep.md", ReadKindAgent, "dev1")
	srv.Reads.Record(p.ID, "top.md", ReadKindAgent, "dev2")
	srv.Reads.Record(p.ID, "wiki/plan.md", ReadKindHuman, "alice@x.io")
	srv.Reads.Record(p.ID, "wiki/plan.md", ReadKindShare, "tok123/1.2.3.4")

	h := srv.Handler()
	base := "/api/p/" + p.ID + "/"
	rec := do(t, h, "GET", base+"heat?by=device&days=30", nil)
	if rec.Code != 200 {
		t.Fatalf("by=device: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Devices []deviceHeat `json:"devices"`
		Since   string       `json:"since"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Devices) != 2 || out.Since == "" {
		t.Fatalf("devices = %+v", out)
	}
	// Sorted by total: dev1 (2 reads, registry-joined) first.
	d1 := out.Devices[0]
	if d1.ID != "dev1" || d1.Name != "ci-agent" || d1.OS != "linux/amd64" || d1.Total != 2 {
		t.Fatalf("dev1 row = %+v", d1)
	}
	if d1.Folders["wiki"] != 2 {
		t.Fatalf("dev1 folders = %+v, want wiki:2", d1.Folders)
	}
	// Root files land under the "" folder.
	if d2 := out.Devices[1]; d2.ID != "dev2" || d2.Folders[""] != 1 {
		t.Fatalf("dev2 row = %+v", d2)
	}
	// The privacy line: human and share actors are invisible here.
	body := rec.Body.String()
	for _, leak := range []string{"alice@x.io", "tok123", "1.2.3.4", "human", "share"} {
		if strings.Contains(body, leak) {
			t.Fatalf("by=device leaked %q: %s", leak, body)
		}
	}

	if rec := do(t, h, "GET", base+"heat?by=path", nil); rec.Code != 400 {
		t.Fatalf("invalid by: %d, want 400", rec.Code)
	}
}
