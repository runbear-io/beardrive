package webapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// The index's guard is a byte comparison standing in for journal.Less, so the
// two must agree on every pair — ties included, which is where a forged
// journal would try to win.
func TestWinKeyOrdersExactlyLikeLess(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	pick := func(vals ...string) string { return vals[rng.IntN(len(vals))] }
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	ops := make([]journal.Op, 400)
	for i := range ops {
		ops[i] = journal.Op{
			Lamport: int64(rng.IntN(3)) - 1,
			Time:    t0.Add(time.Duration(rng.IntN(3)) * time.Nanosecond).Add(time.Duration(rng.IntN(2)) * time.Hour),
			Device:  pick("a", "ab", "a\x00", "b", ""),
			Seq:     int64(rng.IntN(3)),
			Kind:    pick(journal.KindPut, journal.KindDelete),
			Path:    "p.json",
			Blob:    pick("", "aa", "ab"),
			Size:    int64(rng.IntN(2)),
			Mode:    uint32(rng.IntN(2)),
		}
	}
	for _, a := range ops {
		for _, b := range ops {
			want := 0
			if journal.Less(a, b) {
				want = -1
			} else if journal.Less(b, a) {
				want = 1
			}
			if got := bytes.Compare(winKey(a), winKey(b)); got != want {
				t.Fatalf("winKey compares %d, Less says %d:\n%+v\n%+v", got, want, a, b)
			}
		}
	}
}

// dataBackends is every database the index runs on: SQLite always, Postgres
// when BDRIVE_TEST_POSTGRES names one (CI's meta-postgres job).
func dataBackends(t *testing.T) map[string]func(*testing.T) *DataIndex {
	b := map[string]func(*testing.T) *DataIndex{
		"sqlite": func(t *testing.T) *DataIndex {
			x, err := OpenDataIndex("sqlite", filepath.Join(t.TempDir(), "data.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { x.Close() })
			return x
		},
	}
	if dsn := os.Getenv("BDRIVE_TEST_POSTGRES"); dsn != "" {
		b["postgres"] = func(t *testing.T) *DataIndex {
			x, err := OpenDataIndex("pgx", dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { x.Close() })
			return x
		}
	}
	return b
}

// An older fold must never write over a newer one, and a deletion must hold
// against the version it deleted.
func TestDataIndexSQLKeepsTheNewerRow(t *testing.T) {
	for name, open := range dataBackends(t) {
		t.Run(name, func(t *testing.T) {
			x := open(t)
			ctx := context.Background()
			project := "p-guard-" + name + time.Now().Format("150405.000000")
			old := journal.Op{Lamport: 1, Device: "d", Seq: 1, Kind: journal.KindPut, Path: "a.json", Blob: "aa"}
			newer := old
			newer.Lamport, newer.Blob = 2, "bb"
			del := newer
			del.Lamport, del.Kind, del.Blob = 3, journal.KindDelete, ""
			row := func(op journal.Op, doc string) dataRow {
				return dataRow{path: op.Path, win: winKey(op), sha: op.Blob, doc: []byte(doc), deleted: op.Kind == journal.KindDelete}
			}
			for i, rows := range [][]dataRow{{row(newer, `{"v":2}`)}, {row(old, `{"v":1}`)}} {
				refused, err := x.write(ctx, project, rows)
				if err != nil {
					t.Fatal(err)
				}
				if refused != i {
					t.Fatalf("write %d refused %d rows, want %d", i, refused, i)
				}
			}
			if k, _ := x.known(ctx, project); k["a.json"].win != string(winKey(newer)) {
				t.Fatal("an older fold wrote over the newer row")
			}
			for _, rows := range [][]dataRow{{{path: "a.json", win: winKey(del), deleted: true}}, {row(newer, `{"v":2}`)}} {
				if _, err := x.write(ctx, project, rows); err != nil {
					t.Fatal(err)
				}
			}
			if k, _ := x.known(ctx, project); !k["a.json"].deleted {
				t.Fatal("a fold from before the delete put the file back")
			}
		})
	}
}

// dataHub is permHub with a data index and some files in a project.
type dataHub struct {
	t       *testing.T
	h       http.Handler
	srv     *Server
	cookies map[string]*http.Cookie
	p       Project
}

func newDataHub(t *testing.T, x *DataIndex) *dataHub {
	t.Helper()
	h, srv, cookies, p := permHub(t)
	srv.Data = x
	d := &dataHub{t: t, h: h, srv: srv, cookies: cookies, p: p}
	files := map[string]string{
		"konny/placements/listicle/a.com.json":  `{"domain":"a.com","stage":"Awaiting reply","dr":62,"next_action":{"text":"follow up"},"sends":[{"at":"2026-10-18T23:30:00Z"},{"at":"2026-10-22T12:00:00Z"}]}`,
		"konny/placements/directory/b.com.json": `{"domain":"b.com","stage":"closed","dr":null,"sends":[]}`,
		"konny/placements/listicle/c.com.json":  `{"domain":"c.com","stage":"Closed","dr":"70"}`,
		"acme/placements/editorial/d.com.json":  `{"domain":"d.com","stage":"Awaiting reply","dr":80,"sends":[{"at":"2026-10-20"}]}`,
		"konny/social/reddit/queue.json":        `{"items":[{"id":"x1","status":"sent"},{"id":"x2","status":"draft"}]}`,
		"notes/plan.md":                         "---\nstatus: draft\npriority: 2\n---\nThe plan.\n",
		"notes/free.md":                         "No frontmatter here.\n",
		"konny/placements/listicle/broken.json": `{"domain": oops`,
		"secret/s.json":                         `{"stage":"Awaiting reply","dr":99}`,
		"lit/a_1.json":                          `{"n":1}`,
		"lit/ab1.json":                          `{"n":2}`,
	}
	for path, body := range files {
		d.put(path, body)
	}
	d.send("PUT", "/api/p/"+p.ID+"/folders", `{"prefix":"secret","default":"none"}`, "alice", nil, 200)
	return d
}

func (d *dataHub) send(method, url, body, who string, hdr http.Header, want int) *httptest.ResponseRecorder {
	d.t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.AddCookie(d.cookies[who])
	for k, v := range hdr {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	d.h.ServeHTTP(rec, req)
	if want != 0 && rec.Code != want {
		d.t.Fatalf("%s %s as %s: %d %s, want %d", method, url, who, rec.Code, rec.Body, want)
	}
	return rec
}

func (d *dataHub) put(path, body string) {
	d.t.Helper()
	d.send("PUT", "/api/p/"+d.p.ID+"/upload/content?path="+path, body, "alice", nil, 200)
}

type dataAnswer struct {
	Rows     []map[string]json.RawMessage `json:"rows"`
	Groups   []map[string]json.RawMessage `json:"groups"`
	Count    int                          `json:"count"`
	Next     string                       `json:"next"`
	Errors   int                          `json:"errors"`
	Complete bool                         `json:"complete"`
	etag     string
}

func (d *dataHub) query(who string, q map[string]any) dataAnswer {
	d.t.Helper()
	body, _ := json.Marshal(q)
	rec := d.send("POST", "/api/p/"+d.p.ID+"/data/query", string(body), who, nil, 200)
	var a dataAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		d.t.Fatalf("answer: %v %s", err, rec.Body)
	}
	a.etag = rec.Header().Get("ETag")
	return a
}

// enable turns data on and waits for the first build.
func (d *dataHub) enable() {
	d.t.Helper()
	d.send("PUT", "/api/p/"+d.p.ID+"/data", `{"enabled":true}`, "alice", nil, 200)
	deadline := time.Now().Add(10 * time.Second)
	for !d.query("alice", map[string]any{"pattern": "**/*.json", "count": true}).Complete {
		if time.Now().After(deadline) {
			d.t.Fatal("the first build never completed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func strs(rows []map[string]json.RawMessage, key string) []string {
	var out []string
	for _, r := range rows {
		out = append(out, string(r[key]))
	}
	return out
}

func TestDataQuerySQLAnswersFromTheIndex(t *testing.T) {
	for name, open := range dataBackends(t) {
		t.Run(name, func(t *testing.T) {
			d := newDataHub(t, open(t))
			d.send("POST", "/api/p/"+d.p.ID+"/data/query", `{"pattern":"**/*.json"}`, "alice", nil, http.StatusNotFound)
			d.send("PUT", "/api/p/"+d.p.ID+"/data", `{"enabled":true}`, "bob", nil, http.StatusForbidden)
			d.enable()
			placements := "{client}/placements/**/*.json"

			// A kanban: one client, chosen fields, typed order. Numbers sort by
			// value, then strings, then nulls; the broken file is counted, not
			// listed.
			k := d.query("alice", map[string]any{
				"pattern": placements, "where": map[string]any{"$client": "konny"},
				"select": []string{"domain", "dr", "next_action.text"}, "order": []string{"-dr"},
			})
			if got := strings.Join(strs(k.Rows, "domain"), ","); got != `"a.com","c.com","b.com"` {
				t.Fatalf("kanban order = %s", got)
			}
			if string(k.Rows[0]["next_action.text"]) != `"follow up"` || k.Errors != 1 {
				t.Fatalf("kanban row = %v, errors %d", k.Rows[0], k.Errors)
			}

			// Counts by client and stage, folding the case the files disagree on.
			g := d.query("alice", map[string]any{"pattern": placements, "group": []string{"$client", "lower(stage)"}})
			counts := map[string]string{}
			for _, r := range g.Groups {
				counts[string(r["$client"])+" "+string(r["lower(stage)"])] = string(r["count"])
			}
			if counts[`"konny" "closed"`] != "2" || counts[`"konny" "awaiting reply"`] != "1" || counts[`"acme" "awaiting reply"`] != "1" {
				t.Fatalf("counts = %v", counts)
			}

			// Contacts per client per week, in the reader's zone: a Sunday
			// 23:30 UTC send is a Monday in Seoul.
			w := d.query("alice", map[string]any{
				"pattern": placements, "each": "/sends",
				"group": []string{"$client", "week(at, Asia/Seoul)"},
			})
			kst, _ := isoWeek("2026-10-18T23:30:00Z", "Asia/Seoul")
			utc, _ := isoWeek("2026-10-18T23:30:00Z", "UTC")
			weeks := map[string]string{}
			for _, r := range w.Groups {
				weeks[string(r["$client"])+" "+string(r["week(at, Asia/Seoul)"])] = string(r["count"])
			}
			if kst == utc || weeks[`"konny" "`+kst+`"`] == "" || weeks[`"acme" "2026-W43"`] != "1" {
				t.Fatalf("weeks = %v (KST %s)", weeks, kst)
			}

			// "Awaiting reply and DR over 50": a string "70" is not over 50,
			// and a folder bob is denied is not in his answer at all.
			f := map[string]any{"pattern": "**/*.json", "where": map[string]any{"stage": "Awaiting reply", "dr": map[string]any{"gt": 50.0}}}
			if got := len(d.query("alice", f).Rows); got != 3 {
				t.Fatalf("alice sees %d awaiting, want 3", got)
			}
			bob := d.query("bob", f)
			for _, p := range strs(bob.Rows, "$path") {
				if strings.Contains(p, "secret/") {
					t.Fatalf("bob's answer names %s", p)
				}
			}
			if len(bob.Rows) != 2 {
				t.Fatalf("bob sees %d awaiting, want 2", len(bob.Rows))
			}

			// Items of one file, and Markdown frontmatter, typed.
			q := d.query("alice", map[string]any{"pattern": "{client}/social/reddit/queue.json", "each": "/items",
				"where": map[string]any{"status": "sent"}, "select": []string{"id", "$item"}})
			if len(q.Rows) != 1 || string(q.Rows[0]["id"]) != `"x1"` || string(q.Rows[0]["$item"]) != `"/items/0"` {
				t.Fatalf("queue items = %v", q.Rows)
			}
			md := d.query("alice", map[string]any{"pattern": "notes/*.md", "where": map[string]any{"priority": map[string]any{"gte": 2.0}}, "select": []string{"status"}})
			if len(md.Rows) != 1 || string(md.Rows[0]["status"]) != `"draft"` {
				t.Fatalf("frontmatter rows = %v", md.Rows)
			}
			// A wildcard segment matches literally around its wildcard.
			if got := strs(d.query("alice", map[string]any{"pattern": "lit/a_*.json"}).Rows, "$path"); len(got) != 1 || got[0] != `"lit/a_1.json"` {
				t.Fatalf("lit/a_*.json matched %v", got)
			}
		})
	}
}

func TestDataQuerySQLETagFollowsOnlyWhatTheReaderSees(t *testing.T) {
	for name, open := range dataBackends(t) {
		t.Run(name, func(t *testing.T) {
			d := newDataHub(t, open(t))
			d.enable()
			body := `{"pattern":"**/*.json","where":{"dr":{"gt":50}},"select":["domain"]}`
			url := "/api/p/" + d.p.ID + "/data/query"
			again := func(who, etag string) int {
				return d.send("POST", url, body, who, http.Header{"If-None-Match": {etag}}, 0).Code
			}
			bob := d.send("POST", url, body, "bob", nil, 200).Header().Get("ETag")
			alice := d.send("POST", url, body, "alice", nil, 200).Header().Get("ETag")
			if bob == "" || bob == alice {
				t.Fatalf("ETags: bob %q, alice %q — they read different rows", bob, alice)
			}
			// A change to a file outside the pattern, and one behind bob's
			// folder rule, leave bob's answer — and so his ETag — alone.
			d.put("notes/other.md", "---\nx: 1\n---\n")
			d.put("secret/s.json", `{"stage":"Closed","dr":99}`)
			if code := again("bob", bob); code != http.StatusNotModified {
				t.Fatalf("bob after a hidden change: %d, want 304", code)
			}
			if code := again("alice", alice); code != http.StatusOK {
				t.Fatalf("alice after a change she can see: %d, want 200", code)
			}
			// A write through the hub is in the index before its request
			// returns: the writer reads it back at once.
			d.put("konny/placements/listicle/a.com.json", `{"domain":"a.com","stage":"Replied","dr":10}`)
			if code := again("bob", bob); code != http.StatusOK {
				t.Fatalf("bob after a visible change: %d, want 200", code)
			}
			if got := strs(d.query("bob", map[string]any{"pattern": "**/*.json", "where": map[string]any{"stage": "Replied"}, "select": []string{"domain"}}).Rows, "domain"); len(got) != 1 {
				t.Fatalf("the write was not readable at once: %v", got)
			}
			// A deletion leaves the answer.
			d.send("POST", "/api/p/"+d.p.ID+"/remove", `{"path":"acme/placements/editorial/d.com.json"}`, "alice", nil, 200)
			if got := d.query("alice", map[string]any{"pattern": "acme/**/*.json", "count": true}).Count; got != 0 {
				t.Fatalf("a deleted file is still counted: %d", got)
			}
		})
	}
}

func TestDataQuerySQLPagesHoldToOneVersion(t *testing.T) {
	for name, open := range dataBackends(t) {
		t.Run(name, func(t *testing.T) {
			d := newDataHub(t, open(t))
			d.enable()
			q := map[string]any{"pattern": "{client}/placements/**/*.json", "limit": 2}
			first := d.query("alice", q)
			if len(first.Rows) != 2 || first.Next == "" {
				t.Fatalf("first page = %d rows, next %q", len(first.Rows), first.Next)
			}
			q["cursor"] = first.Next
			second := d.query("alice", q)
			if len(second.Rows) != 2 || second.Next != "" || string(second.Rows[0]["$path"]) == string(first.Rows[0]["$path"]) {
				t.Fatalf("second page = %v", second.Rows)
			}
			d.put("acme/placements/editorial/e.com.json", `{"domain":"e.com"}`)
			body, _ := json.Marshal(q)
			d.send("POST", "/api/p/"+d.p.ID+"/data/query", string(body), "alice", nil, http.StatusConflict)
			d.send("POST", "/api/p/"+d.p.ID+"/data/query", `{"pattern":"**/*.json","limit":1001}`, "alice", nil, http.StatusBadRequest)
		})
	}
}

func TestDataQuerySQLDescribeAndOff(t *testing.T) {
	for name, open := range dataBackends(t) {
		t.Run(name, func(t *testing.T) {
			d := newDataHub(t, open(t))
			d.enable()
			rec := d.send("POST", "/api/p/"+d.p.ID+"/data/describe", `{"pattern":"{client}/placements/**/*.json","field":"next_action"}`, "alice", nil, 200)
			var out struct {
				Fields []fieldStat `json:"fields"`
			}
			json.Unmarshal(rec.Body.Bytes(), &out)
			if len(out.Fields) != 1 || out.Fields[0].Path != "next_action.text" || out.Fields[0].Types["string"] != 1 {
				t.Fatalf("describe next_action = %+v", out.Fields)
			}
			d.send("PUT", "/api/p/"+d.p.ID+"/data", `{"enabled":false}`, "alice", nil, 200)
			d.send("POST", "/api/p/"+d.p.ID+"/data/query", `{"pattern":"**/*.json"}`, "alice", nil, http.StatusNotFound)
			if k, _ := d.srv.Data.known(context.Background(), d.p.ID); len(k) != 0 {
				t.Fatalf("turning data off left %d rows", len(k))
			}
		})
	}
}

func TestMCPQueryUsesTheSameQuery(t *testing.T) {
	f := newMCPHub(t)
	x, err := OpenDataIndex("sqlite", filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	f.srv.Data = x
	cs := f.session(f.connect("alice", f.wiki.ID))
	mustCall(t, cs, "write", map[string]any{"path": "/" + f.wiki.ID + "/a.json", "content": `{"stage":"open"}`})
	d := &dataHub{t: t, h: f.h, srv: f.srv, cookies: f.cookies, p: f.wiki}
	d.enable()
	out := mustCall(t, cs, "query", map[string]any{"project": f.wiki.ID, "pattern": "*.json", "select": []string{"stage"}})
	if !strings.Contains(out, `"stage":"open"`) {
		t.Fatalf("query tool = %s", out)
	}
	out = mustCall(t, cs, "describe", map[string]any{"project": "wiki", "pattern": "*.json"})
	if !strings.Contains(out, `"path":"stage"`) {
		t.Fatalf("describe tool = %s", out)
	}
}

// A device's push reaches the index through the fold, not through a hub
// door: here, ops land in storage behind the hub's back, and the pass a
// process runs after boot picks them up, deletions included.
func TestDataQuerySQLFollowsTheFoldAfterBoot(t *testing.T) {
	h, srv, cookies, p, root := permHubAt(t)
	x, err := OpenDataIndex("sqlite", filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	srv.Data = x
	reads, err := OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	srv.Reads = reads
	d := &dataHub{t: t, h: h, srv: srv, cookies: cookies, p: p}
	d.enable()

	dev := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	dev.put("laptop", "x/a.json", `{"v":1}`)
	count := func() int { return d.query("alice", map[string]any{"pattern": "x/*.json", "count": true}).Count }
	waitFor := func(want int) {
		t.Helper()
		srv.dataBoot()
		deadline := time.Now().Add(5 * time.Second)
		for count() != want {
			if time.Now().After(deadline) {
				t.Fatalf("the index never reached %d rows", want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor(1)

	// A projection is not a read of the file; the whole document is.
	d.query("alice", map[string]any{"pattern": "x/*.json", "select": []string{"v"}})
	if got := reads.Heat(p.ID, "", time.Time{})["x/a.json"].Human; got != 0 {
		t.Fatalf("a projection counted %d reads", got)
	}
	d.query("alice", map[string]any{"pattern": "x/*.json", "select": []string{"$doc"}})
	if got := reads.Heat(p.ID, "", time.Time{})["x/a.json"].Human; got != 1 {
		t.Fatalf("reading the whole document counted %d reads, want 1", got)
	}

	dev.del("laptop", "x/a.json")
	waitFor(0)
}

// A pass writes a slice at a time; with slices of two files every file still
// lands, and a later change still replaces its row.
func TestDataQuerySQLAppliesInSlices(t *testing.T) {
	files, bytes := dataApplyChunkFiles, dataApplyChunkBytes
	dataApplyChunkFiles, dataApplyChunkBytes = 2, 1<<20
	defer func() { dataApplyChunkFiles, dataApplyChunkBytes = files, bytes }()
	x, err := OpenDataIndex("sqlite", filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	x.SetMaxConns(1, 1)
	d := newDataHub(t, x)
	d.enable()
	if got := d.query("alice", map[string]any{"pattern": "**/*.json", "count": true}); got.Count != 8 || got.Errors != 1 {
		t.Fatalf("indexed %d files with %d errors, want 8 and 1", got.Count, got.Errors)
	}
	d.put("lit/ab1.json", `{"n":3}`)
	if got := d.query("alice", map[string]any{"pattern": "lit/ab1.json", "select": []string{"n"}}); len(got.Rows) != 1 || string(got.Rows[0]["n"]) != "3" {
		t.Fatalf("after a change: %v", got.Rows)
	}
}

// A JSON file that becomes a link stops being data. Its row is removed under
// the key of the link that replaced it — not under an older delete in its
// history, which the index would refuse while the applier went on believing
// the row was gone. Review (runbear-bot) on #320.
func TestDataQuerySQLDropsAFileThatBecameALink(t *testing.T) {
	h, srv, cookies, p, root := permHubAt(t)
	x, err := OpenDataIndex("sqlite", filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	srv.Data = x
	d := &dataHub{t: t, h: h, srv: srv, cookies: cookies, p: p}
	d.enable()
	dev := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	count := func() int { return d.query("alice", map[string]any{"pattern": "x/*.json", "count": true}).Count }
	settle := func(want int) {
		t.Helper()
		srv.dataBoot()
		deadline := time.Now().Add(5 * time.Second)
		for count() != want {
			if time.Now().After(deadline) {
				t.Fatalf("the index never reached %d rows", want)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	// A delete in the path's history, older than the version indexed now.
	dev.put("laptop", "x/a.json", `{"v":1}`)
	dev.del("laptop", "x/a.json")
	dev.put("laptop", "x/a.json", `{"v":2}`)
	settle(1)
	dev.append("laptop", journal.Op{Kind: journal.KindPut, Path: "x/a.json", Link: "elsewhere.json"})
	settle(0)
	// And it stays gone: a later pass does not skip it, and nothing brings it back.
	srv.dataBoot()
	time.Sleep(100 * time.Millisecond)
	if got := count(); got != 0 {
		t.Fatalf("the link's path came back with %d rows", got)
	}
}

// A row another process indexed from a newer fold stays when this process's
// fold has not caught up with it: no delete ended it, so nothing here may
// remove it, and removing it would let an older fold put back its own row.
func TestDataQuerySQLKeepsARowItsFoldPredates(t *testing.T) {
	for name, open := range dataBackends(t) {
		t.Run(name, func(t *testing.T) {
			x := open(t)
			d := newDataHub(t, x)
			d.enable()
			op := journal.Op{
				Lamport: 1 << 40, Time: time.Now().UTC(), Device: "elsewhere", Seq: 1,
				Kind: journal.KindPut, Path: "y/new.json", Blob: strings.Repeat("a", 64), Size: 7,
			}
			row := dataRow{path: op.Path, win: winKey(op), sha: op.Blob, changedAt: op.Time, doc: []byte(`{"n":1}`)}
			if _, err := x.write(context.Background(), d.p.ID, []dataRow{row}); err != nil {
				t.Fatal(err)
			}
			// Loaded after this process's fold was read, as on a pass that raced the other.
			st := d.srv.dataProjectState(d.p.ID)
			st.mu.Lock()
			st.known = nil
			st.mu.Unlock()
			d.srv.dataWrote(context.Background(), d.p.ID)
			if got := d.query("alice", map[string]any{"pattern": "y/*.json", "count": true}).Count; got != 1 {
				t.Fatalf("a row from a newer fold was removed: %d rows", got)
			}
		})
	}
}

// A pass that could not fetch a file reports the index behind until a later
// pass fetches it.
func TestDataQuerySQLReportsAPassThatMissedAFile(t *testing.T) {
	h, srv, cookies, p, root := permHubAt(t)
	x, err := OpenDataIndex("sqlite", filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	srv.Data = x
	d := &dataHub{t: t, h: h, srv: srv, cookies: cookies, p: p}
	d.enable()
	dev := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	body := `{"v":1}`
	sum := sha256.Sum256([]byte(body))
	blob := hex.EncodeToString(sum[:])
	dev.append("laptop", journal.Op{Kind: journal.KindPut, Path: "x/a.json", Blob: blob, Size: int64(len(body))})
	wait := func(what string, ok func(dataAnswer) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			srv.dataBoot()
			if ok(d.query("alice", map[string]any{"pattern": "x/*.json", "count": true})) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	wait("a pass that could not fetch a file still reports the index complete",
		func(a dataAnswer) bool { return !a.Complete })
	if err := os.WriteFile(filepath.Join(root, p.ID, "blobs", blob), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	wait("the index never caught up once the file could be fetched",
		func(a dataAnswer) bool { return a.Complete && a.Count == 1 })
}

// A document neither dialect can store the same way (a NUL, an unpaired
// surrogate, bytes that are not UTF-8) is an error row on both. On Postgres it
// used to fail its whole slice on every pass, so nothing after it was indexed.
func TestDataQuerySQLRefusesWhatTheIndexCannotHold(t *testing.T) {
	for name, open := range dataBackends(t) {
		t.Run(name, func(t *testing.T) {
			d := newDataHub(t, open(t))
			d.enable()
			d.put("z/a-good.json", `{"n":1}`)
			d.put("z/b-nul.json", `{"s":"a\u0000b"}`)
			d.put("z/c-surrogate.json", `{"s":"\ud800"}`)
			d.put("z/d-latin1.json", "{\"s\":\"caf\xe9\"}")
			d.put("z/e-escaped.json", `{"s":"a\\u0000b","t":"café"}`)
			d.put("z/f-nul.md", "---\ns: \"a\\0b\"\n---\nbody\n")
			d.put("z/g-later.json", `{"n":2}`)
			a := d.query("alice", map[string]any{"pattern": "z/*", "select": []string{"$path"}})
			if got := strs(a.Rows, "$path"); strings.Join(got, ",") != `"z/a-good.json","z/e-escaped.json","z/g-later.json"` || a.Errors != 4 {
				t.Fatalf("rows %v errors %d; want the three holdable files and four errors", got, a.Errors)
			}
		})
	}
}

// A write through the hub does not wait for a pass already running (a first
// build can take minutes); the pass it queues indexes it.
func TestDataQuerySQLHubWritesDoNotWaitForAPass(t *testing.T) {
	x, err := OpenDataIndex("sqlite", filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	d := newDataHub(t, x)
	d.enable()
	st := d.srv.dataProjectState(d.p.ID)
	st.mu.Lock() // a pass in progress
	done := make(chan int, 1)
	go func() {
		done <- d.send("PUT", "/api/p/"+d.p.ID+"/upload/content?path=w/new.json", `{"n":1}`, "alice", nil, 0).Code
	}()
	select {
	case code := <-done:
		if code != 200 {
			st.mu.Unlock()
			t.Fatalf("write: %d", code)
		}
	case <-time.After(3 * time.Second):
		st.mu.Unlock()
		t.Fatal("a hub write waited for the running pass")
	}
	st.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for d.query("alice", map[string]any{"pattern": "w/*.json", "count": true}).Count != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the queued pass never indexed the write")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A query's size is bounded where it is compiled, so an oversized one is the
// caller's 400 rather than the database's error (a 502 that reads as the
// hub's fault).
func TestDataQueryBoundsItsSize(t *testing.T) {
	x, err := OpenDataIndex("sqlite", filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	d := newDataHub(t, x)
	d.enable()
	many := make([]any, dataMaxTerms+1)
	for i := range many {
		many[i] = float64(i)
	}
	deep := map[string]any{"dr": 1.0}
	for range dataMaxDepth + 1 {
		deep = map[string]any{"not": deep}
	}
	fields := make([]string, dataMaxFields+1)
	for i := range fields {
		fields[i] = "f" + strconv.Itoa(i)
	}
	for name, q := range map[string]map[string]any{
		"a long in list":  {"pattern": "**/*.json", "where": map[string]any{"dr": map[string]any{"in": many}}},
		"deep nesting":    {"pattern": "**/*.json", "where": deep},
		"too many fields": {"pattern": "**/*.json", "select": fields},
		"the hub's zone":  {"pattern": "**/*.json", "group": []string{"week(sends.0.at, Local)"}},
		"an empty zone":   {"pattern": "**/*.json", "group": []string{"week(sends.0.at, )"}},
	} {
		body, _ := json.Marshal(q)
		if rec := d.send("POST", "/api/p/"+d.p.ID+"/data/query", string(body), "alice", nil, 0); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, rec.Code, rec.Body)
		}
	}
}
