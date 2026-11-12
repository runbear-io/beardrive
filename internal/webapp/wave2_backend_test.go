package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/cluster"

	"github.com/runbear-io/beardrive/internal/remote"
)

func TestWave2ReceiptSourcesShareOneCap(t *testing.T) {
	r := httptest.NewRequest("GET", "/?r=query:2,other:3&r=last:4,overflow:5", nil)
	r.Header.Add(receiptHeader, "header:1")
	got := parseReceipts(r)
	if len(got) != 4 || got[0].String() != "header:1" || got[3].String() != "last:4" {
		t.Fatalf("receipts: %+v", got)
	}
	r = httptest.NewRequest("GET", "/?r=bad:0,no%20device:2,good:3", nil)
	if got = parseReceipts(r); len(got) != 1 || got[0].String() != "good:3" {
		t.Fatalf("invalid receipt accepted: %+v", got)
	}
}

func TestWave2RawFrameQueryReceiptRevalidates(t *testing.T) {
	a, b, p, _ := newRYOWHubPair(t)
	base := "/api/p/" + p.ID
	writeReceipt(t, a.call(t, "PUT", base+"/upload/content?path=site/index.html", []byte("<p>old</p>"), nil), "seed")
	b.call(t, "GET", base+"/tree", nil, nil)
	receipt := writeReceipt(t, a.call(t, "PUT", base+"/upload/content?path=site/index.html", []byte("<p>new</p>"), nil), "edit")
	token, err := b.s.signFrame(context.Background(), frameClaims{Project: p.ID, Dir: "site", Email: "editor@example.com", Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	_, v, err := b.s.projectVolume(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", base+"/raw/"+token+"/site/index.html?path=outside.html&r="+url.QueryEscape(receipt), nil)
	req.SetPathValue("project", p.ID)
	req.SetPathValue("token", token)
	req.SetPathValue("path", "site/index.html")
	req.Header.Set("If-None-Match", `"`+shaOf("<p>old</p>")+`"`)
	rec := httptest.NewRecorder()
	b.s.handleRaw(v, rec, req)
	if rec.Code != 200 || rec.Body.String() != "<p>new</p>" {
		t.Fatalf("raw receipt: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "private, no-cache" || !strings.Contains(strings.Join(rec.Header().Values("Vary"), ","), receiptHeader) {
		t.Fatalf("frame caching: %v", rec.Header())
	}
}

func TestWave2FreshShareRefoldsAndUnwrapsWiki(t *testing.T) {
	a, b, p, be := newRYOWHubPair(t)
	repo := newFileShareRepo(filepath.Join(t.TempDir(), "shares.json"))
	var err error
	a.s.Shares, err = NewShareDB(repo)
	if err != nil {
		t.Fatal(err)
	}
	b.s.Shares, err = NewShareDB(newFileShareRepo(repo.path))
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/p/" + p.ID
	b.call(t, "GET", base+"/tree", nil, nil) // cached empty fold predates the file
	writeReceipt(t, a.call(t, "PUT", base+"/upload/content?path=note.md", []byte("# Public\n[[private|Readable & safe]]\n"), nil), "create")
	sh, err := a.s.Shares.Create(p.ID, "note.md", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/s/"+sh.Token, nil)
	req.SetPathValue("token", sh.Token)
	rec := httptest.NewRecorder()
	b.s.handleShared(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `<span class="wiki-text">Readable &amp; safe</span>`) || strings.Contains(rec.Body.String(), `href="wiki:`) {
		t.Fatalf("fresh public share: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatal("share lost sandbox")
	}
	// A still-live share record whose file is absent costs only one fresh fold.
	missing, err := a.s.Shares.Create(p.ID, "deleted.md", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	before := be.journalLists.Load()
	for range 8 {
		req := httptest.NewRequest("GET", "/s/"+missing.Token, nil)
		req.SetPathValue("token", missing.Token)
		rec := httptest.NewRecorder()
		b.s.handleShared(rec, req)
		if rec.Code != 404 {
			t.Fatalf("missing share: %d", rec.Code)
		}
	}
	if n := be.journalLists.Load() - before; n > 1 {
		t.Fatalf("negative share caused %d journal listings", n)
	}
	_, v, _ := b.s.projectVolume(p.ID)
	for i := range maxUnseenReceipts + 20 {
		v.unseenMu.Lock()
		v.shareMiss = insertUnseen(v.shareMiss, fmt.Sprint(i), time.Now(), receiptMissCooldown)
		v.unseenMu.Unlock()
	}
	if len(v.shareMiss) > 256 {
		t.Fatal("unbounded share miss cache")
	}
}

func TestWave2RestoreOlderAuthoredBlobIntoSiblingLiveRoom(t *testing.T) {
	// Drive snapshots explicitly; timer snapshots must not obscure causal order.
	fastRoom(t, time.Hour, time.Hour)
	a, b, p := newHubPair(t)
	ha, hb := a.Handler(), b.Handler()
	base := "/api/p/" + p.ID
	room := p.ID + "/notes.md"
	putContent(t, ha, base+"/upload/content?path=notes.md", "base\n", "")
	doc := liveRoom(t, b, room, true)
	typeInto(doc, "version two\n")
	b.snapshotRoom(context.Background(), room)
	v2 := doc.GetText("body").ToString()
	sha2 := shaOf(v2)
	typeInto(doc, "version three\n")
	b.snapshotRoom(context.Background(), room)
	if !b.contains(context.Background(), doc, p.ID, sha2) {
		t.Fatal("test did not retain v2 Yjs history")
	}
	head := b.roomHeadRecord(context.Background(), p.ID, "notes.md")
	restoreVia(t, ha, p.ID, "notes.md", sha2)
	_, fi, have, err := b.roomFile(context.Background(), room)
	if err != nil || !have || fileWin(fi, "notes.md") <= head.Win || fi.Blob == head.SHA {
		t.Fatalf("restore provenance not newer: %+v %+v %v", fi, head, err)
	}
	b.snapshotRoom(context.Background(), room)
	if got := doc.GetText("body").ToString(); got != v2 {
		t.Fatalf("room undid restore: %q, want %q", got, v2)
	}
	if got := get(t, hb, base+"/file?path=notes.md").Body.String(); got != v2 {
		t.Fatalf("storage undid restore: %q", got)
	}
	typeInto(doc, "after restore\n")
	b.snapshotRoom(context.Background(), room)
	if got := get(t, hb, base+"/file?path=notes.md").Body.String(); got != v2+"after restore\n" {
		t.Fatalf("post-restore edit: %q", got)
	}
	if n := conflictCopies(t, hb, p.ID); n != 0 {
		t.Fatalf("clean restore parked %d copies", n)
	}
}

func TestWave2MetadataAtomicity(t *testing.T) {
	for _, backend := range metaBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			backend.reset(t)
			a, b := backend.open(t), backend.open(t)
			defer a.Close()
			defer b.Close()
			oa, err := NewOrgDB(a.Orgs())
			if err != nil {
				t.Fatal(err)
			}
			ob, err := NewOrgDB(b.Orgs())
			if err != nil {
				t.Fatal(err)
			}
			for _, remove := range []bool{false, true} {
				o, err := oa.Create(fmt.Sprint("owners-", remove), "alice@example.com")
				if err != nil {
					t.Fatal(err)
				}
				if err := oa.AddMember(o.ID, "bob@example.com", RoleOwner); err != nil {
					t.Fatal(err)
				}
				start := make(chan struct{})
				errs := make(chan error, 2)
				for i, db := range []*OrgDB{oa, ob} {
					email := []string{"alice@example.com", "bob@example.com"}[i]
					go func(db *OrgDB, email string) {
						<-start
						if remove {
							errs <- db.RemoveMember(o.ID, email)
						} else {
							errs <- db.SetRole(o.ID, email, RoleMember)
						}
					}(db, email)
				}
				close(start)
				successes := 0
				for range 2 {
					err := <-errs
					if err == nil {
						successes++
					} else if !errors.Is(err, ErrLastOwner) {
						t.Fatal(err)
					}
				}
				if successes != 1 {
					t.Fatalf("concurrent owner mutations: %d successes", successes)
				}
				got, _ := oa.Get(o.ID)
				if oa.ownerCount(got) != 1 {
					t.Fatalf("owners after race: %+v", got.Members)
				}
			}
			// Deterministic personal orgs coalesce without overwriting memberships.
			start := make(chan struct{})
			ids := make(chan string, 2)
			errs := make(chan error, 2)
			for _, db := range []*OrgDB{oa, ob} {
				go func(db *OrgDB) {
					<-start
					o, err := db.CreatePersonal("Personal", "PERSON@example.com ")
					ids <- o.ID
					errs <- err
				}(db)
			}
			close(start)
			for range 2 {
				if err := <-errs; err != nil {
					t.Fatal(err)
				}
			}
			if x, y := <-ids, <-ids; x != y {
				t.Fatalf("personal org split: %s %s", x, y)
			}
			pa, err := NewProjectDB(a.Projects())
			if err != nil {
				t.Fatal(err)
			}
			pb, err := NewProjectDB(b.Projects())
			if err != nil {
				t.Fatal(err)
			}
			start = make(chan struct{})
			ids = make(chan string, 12)
			errs = make(chan error, 12)
			for i := range 12 {
				db := pa
				if i%2 != 0 {
					db = pb
				}
				go func(db *ProjectDB, i int) {
					<-start
					name := "Project"
					if i%2 != 0 {
						name = "project"
					}
					p, _, err := db.GetOrCreate(name, "org-unique")
					ids <- p.ID
					errs <- err
				}(db, i)
			}
			close(start)
			id := ""
			for range 12 {
				if err := <-errs; err != nil {
					t.Fatal(err)
				}
				got := <-ids
				if id != "" && got != id {
					t.Fatalf("duplicate project %s != %s", got, id)
				}
				id = got
			}
			sa, err := NewShareDB(a.Shares())
			if err != nil {
				t.Fatal(err)
			}
			sb, err := NewShareDB(b.Shares())
			if err != nil {
				t.Fatal(err)
			}
			sh, err := sa.Create(id, "note.md", "", 0)
			if err != nil {
				t.Fatal(err)
			}
			sb.Get(sh.Token)
			if !sa.Revoke(sh.Token) {
				t.Fatal("revoke failed")
			}
			if _, _, err := sb.SetExpiry(sh.Token, time.Hour); !errors.Is(err, ErrShareNotFound) {
				t.Fatalf("expiry after revoke: %v", err)
			}
			if err := b.Shares().(shareExpiryUpdater).UpdateExpiry(sh.Token, time.Now()); !errors.Is(err, ErrShareNotFound) {
				t.Fatalf("store resurrected revoked share: %v", err)
			}
			authA, err := NewBuiltinAuth(a.Accounts(), false, nil)
			if err != nil {
				t.Fatal(err)
			}
			authB, err := NewBuiltinAuth(b.Accounts(), false, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := authA.SetPolicy(false, true); err != nil {
				t.Fatal(err)
			}
			if !authB.Policy().RequireApproval {
				t.Fatal("sibling did not refresh saved signup policy")
			}
			// Startup config defaults remain effective when DB holds no new override.
			authB.RequireVerification = true
			if !authB.Policy().RequireVerification {
				t.Fatal("unchanged policy undid pinned startup default")
			}
			la, err := NewReadLedger(a.Reads(), 0)
			if err != nil {
				t.Fatal(err)
			}
			lb, err := NewReadLedger(b.Reads(), 0)
			if err != nil {
				t.Fatal(err)
			}
			la.Record(id, "note.md", ReadKindShare, "same-actor")
			lb.Record(id, "note.md", ReadKindShare, "same-actor")
			if err := la.Close(); err != nil {
				t.Fatal(err)
			}
			if err := lb.Close(); err != nil {
				t.Fatal(err)
			}
			// Refresh has no sleep: reset the throttle as if two seconds elapsed.
			la.mu.Lock()
			la.lastRefresh = time.Time{}
			la.mu.Unlock()
			if got := la.Heat(id, "", time.Time{})["note.md"].Share; got != 2 {
				t.Fatalf("additive sibling count=%d", got)
			}
			la.Record(id, "note.md", ReadKindShare, "unflushed")
			la.mu.Lock()
			la.lastRefresh = time.Time{}
			la.mu.Unlock()
			if got := la.ShareOpens(id)["note.md"].Count; got != 3 {
				t.Fatalf("refresh lost or duplicated local delta: %d", got)
			}
			if err := la.Close(); err != nil {
				t.Fatal(err)
			}
			lb.mu.Lock()
			lb.lastRefresh = time.Time{}
			lb.mu.Unlock()
			if got := lb.Heat(id, "", time.Time{})["note.md"].Share; got != 3 {
				t.Fatalf("refresh after flush=%d", got)
			}
		})
	}
}

// Each child opens a distinct repo instance in a distinct OS process, so a
// process-local mutex cannot make this pass.
func TestWave2FileWriteWorker(t *testing.T) {
	dir := os.Getenv("BDRIVE_WAVE2_FILE_TEST")
	if dir == "" {
		return
	}
	r := newFileShareRepo(filepath.Join(dir, "shares.json"))
	for i := range 12 {
		if err := r.Put(Share{Token: fmt.Sprintf("%s-%d", os.Getenv("BDRIVE_WAVE2_WORKER"), i), Path: "file.md"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWave2FileMetadataCrossProcessWrites(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := range 4 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestWave2FileWriteWorker$")
			cmd.Env = append(os.Environ(), "BDRIVE_WAVE2_FILE_TEST="+dir, fmt.Sprintf("BDRIVE_WAVE2_WORKER=%d", i))
			out, err := cmd.CombinedOutput()
			if err != nil {
				err = fmt.Errorf("child %d: %w: %s", i, err, out)
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := newFileShareRepo(filepath.Join(dir, "shares.json")).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 48 {
		t.Fatalf("lost file metadata writes: %d rows, want 48", len(rows))
	}
}

func TestWave2DataReenableRebuildsSiblingCache(t *testing.T) {
	a, b, p := newHubPair(t)
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "data.db")
	xa, err := OpenDataIndex("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer xa.Close()
	xb, err := OpenDataIndex("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer xb.Close()
	a.Data, b.Data = xa, xb
	if err := a.Projects.SetData(p.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := xa.start(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	putContent(t, a.Handler(), "/api/p/"+p.ID+"/upload/content?path=doc.json", `{"answer":42}`, "")
	st := a.dataProjectState(p.ID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := a.dataPass(ctx, p.ID, st); err != nil {
		t.Fatal(err)
	}
	if len(st.known) == 0 {
		t.Fatal("test did not populate cached winners")
	}
	if err := b.Projects.SetData(p.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := xb.dropProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.Projects.SetData(p.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := xb.start(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.dataPass(ctx, p.ID, st); err != nil {
		t.Fatal(err)
	}
	known, err := xa.known(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := known["doc.json"]; !ok {
		t.Fatal("sibling's old cached winner skipped re-index after off/on")
	}
}

func TestWave2UnicodeHeadingIDs(t *testing.T) {
	out, err := RenderMarkdown([]byte("# Body\n## 한글 제목\n## 한글 제목\n## Café\n## Cafe\u0301\n# Body\n# Body-1\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"body", "한글-제목", "한글-제목-1", "café", "café-1", "body-1", "body-1-1"} {
		if !strings.Contains(out, `id="`+id+`"`) {
			t.Fatalf("missing heading id %q: %s", id, out)
		}
	}
	again, err := RenderMarkdown([]byte("# Body"))
	if err != nil || !strings.Contains(again, `id="body"`) {
		t.Fatal("IDs leaked between documents")
	}
}

func TestWave2UploadRefusesUnicodeAndCaseTwins(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	base := "/api/p/" + p.ID + "/upload/content?path="
	for _, path := range []string{"Café.md", "Plan.md"} {
		if rec := putContent(t, h, base+url.QueryEscape(path), "original", ""); rec.Code != 200 {
			t.Fatalf("seed %s: %d %s", path, rec.Code, rec.Body)
		}
	}
	for _, path := range []string{"Cafe\u0301.md", "plan.md"} {
		if rec := putContent(t, h, base+url.QueryEscape(path), "twin", ""); rec.Code != 409 || !strings.Contains(rec.Body.String(), "macOS/Windows") {
			t.Fatalf("twin %s: %d %s", path, rec.Code, rec.Body)
		}
	}
	if rec := putContent(t, h, base+url.QueryEscape("Café.md"), "replacement", ""); rec.Code != 200 {
		t.Fatalf("exact overwrite blocked: %d %s", rec.Code, rec.Body)
	}
	// Init must reject the same collision before granting a direct upload.
	rec := do(t, h, "POST", "/api/p/"+p.ID+"/upload/init", map[string]any{"path": "plan.md", "sha256": shaOf("twin"), "size": 4})
	if rec.Code != 409 {
		t.Fatalf("init twin: %d %s", rec.Code, rec.Body)
	}
	if got := uploadTwin(map[string]FileInfo{"Plan.md": {}, "plan.md": {}}, "plan.md"); got != "" {
		t.Fatalf("existing exact twin must remain editable: %q", got)
	}
}

func TestWave2DeleteEventsCarryActorAndKind(t *testing.T) {
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Bdrive-Device", "device-1")
	req.Header.Set("X-Bdrive-Device-Name", "Laptop")
	ev, ok := changeEventFor(req, "browser", []string{"gone.md"}, 0, 1)
	if !ok || ev.Kind != "delete" || ev.Device != "device-1" {
		t.Fatalf("delete frame: %+v", ev)
	}
	ev.User = "editor@example.com"
	ev.UserName = "Editor"
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"user":"editor@example.com"`) {
		t.Fatal("delete actor missing from wire")
	}
}

func TestWave2DirectCommitRefusesTwinButKeepsExactOverwrite(t *testing.T) {
	f := newFakeRemote(t)
	f.put("seed", "Plan.md", "old")
	srv := f.uploadServer(func(be remote.Backend) remote.Backend { return &signingBackend{Backend: be} })
	h := srv.Handler()
	// Both checks must run even if callers skip init and call commit directly.
	for _, door := range []string{"init", "commit"} {
		rec := do(t, h, "POST", "/api/upload/"+door, initReq("plan.md", "old"))
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s twin: %d %s", door, rec.Code, rec.Body)
		}
	}
	rec := do(t, h, "POST", "/api/upload/commit", initReq("Plan.md", "old"))
	if rec.Code != http.StatusOK {
		t.Fatalf("exact direct commit: %d %s", rec.Code, rec.Body)
	}
}

type wave2FailingReadAdder struct {
	ReadRepo
	fail bool
}

func (r *wave2FailingReadAdder) AddBatch(d []ReadStat) error {
	if r.fail {
		return os.ErrPermission
	}
	return r.ReadRepo.(readAdder).AddBatch(d)
}

func TestWave2ReadDeltasSurviveFailedFlush(t *testing.T) {
	repo := &wave2FailingReadAdder{ReadRepo: newFileReadRepo(filepath.Join(t.TempDir(), "reads.json")), fail: true}
	l, err := NewReadLedger(repo, 0)
	if err != nil {
		t.Fatal(err)
	}
	l.Record("p", "a.md", ReadKindHuman, "alice")
	if err := l.Close(); err == nil {
		t.Fatal("failed additive flush reported success")
	}
	if len(l.pendingDeltas) != 1 {
		t.Fatal("failed flush discarded its delta")
	}
	l.lastRefresh = time.Time{}
	if got := l.Heat("p", "", time.Time{})["a.md"].Human; got != 1 {
		t.Fatalf("failed-flush overlay: %d", got)
	}
	repo.fail = false
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if len(l.pendingDeltas) != 0 {
		t.Fatal("successful flush kept flushed delta")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Count != 1 {
		t.Fatalf("retry double counted: %+v", stored)
	}
}

func TestWave2RelayPeerAdoptsExactWriterSnapshot(t *testing.T) {
	fastRoom(t, time.Hour, time.Hour)
	a, b, p := newHubPair(t)
	relay := cluster.NewMemRelay()
	for _, s := range []*Server{a, b} {
		if err := s.useCollabRelay(relay); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	base := "/api/p/" + p.ID
	room := p.ID + "/notes.md"
	putContent(t, a.Handler(), base+"/upload/content?path=notes.md", "base\n", "")
	da, db := liveRoom(t, a, room, true), liveRoom(t, b, room, true)
	typeInto(da, "relay version\n")
	// Deliver exactly the update the relay would, before the writer snapshots.
	if err := db.ApplyUpdate(da.EncodeStateAsUpdate()); err != nil {
		t.Fatal(err)
	}
	a.snapshotRoom(ctx, room)
	want := da.GetText("body").ToString()
	b.snapshotRoom(ctx, room) // equality path precedes the non-holder lease check
	if got := db.GetText("body").ToString(); got != want {
		t.Fatalf("relay peer was spliced: %q", got)
	}
	if got := b.rooms().base(room); got != shaOf(want) {
		t.Fatalf("peer did not adopt saved head: %s", got)
	}
	if n := conflictCopies(t, b.Handler(), p.ID); n != 0 {
		t.Fatalf("healthy relay parked %d conflicts", n)
	}
}

func TestWave2FileDeviceRegistryUsesOneLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "devices.json")
	registry, err := OpenDeviceRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	// Observe and Bind both enter withDeviceLock; a second flock descriptor
	// there would wait forever on the fileMu lock this goroutine already owns.
	registry.Observe(DeviceInfo{ID: "observed-device", User: "alice@example.com", Name: "Laptop"})
	if err := registry.Bind("alice@example.com", DeviceInfo{ID: "bound-device", Name: "Desktop"}, nil); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDeviceRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"observed-device", "bound-device"} {
		if !reopened.MayActAs("alice@example.com", id) || reopened.MayActAs("bob@example.com", id) {
			t.Fatalf("device %s ownership did not persist", id)
		}
	}
	// The delete path shares that lock too.
	reopened.Release("alice@example.com")
	if reopened.AnyOwned() {
		t.Fatal("release did not remove persisted device rows")
	}
}
