package webapp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/webapp"
)

type headerAuth struct {
	binder webapp.DeviceBinder
}

func (a *headerAuth) CLILoginPath() string { return "/auth/login" }

func (a *headerAuth) Authenticate(r *http.Request) (webapp.User, bool) {
	email := r.Header.Get("X-Test-User")
	if email == "" {
		return webapp.User{}, false
	}
	return webapp.User{
		ID:    email,
		Email: email,
		Name:  email,
		Admin: email == "alice@example.com",
	}, true
}

func (a *headerAuth) Register(*http.ServeMux) {}

func (a *headerAuth) Accounts() []webapp.User {
	return []webapp.User{
		{ID: "alice@example.com", Email: "alice@example.com", Name: "Alice", Admin: true},
		{ID: "bob@example.com", Email: "bob@example.com", Name: "Bob"},
	}
}

func (a *headerAuth) UseDeviceBinder(bind webapp.DeviceBinder) { a.binder = bind }

type regressionFixture struct {
	handler   http.Handler
	be        remote.Backend
	projDir   string
	projectID string
	shas      map[string]string
}

func sha256HexStr(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func setupRegressionHub(t *testing.T, totalOps int) regressionFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	metaDir := t.TempDir()

	be, err := remote.Open(ctx, "file://"+root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Close() })

	orgs, err := webapp.OpenOrgDB(filepath.Join(metaDir, "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	org, err := orgs.Create("acme", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := orgs.AddMember(org.ID, "bob@example.com", webapp.RoleMember); err != nil {
		t.Fatal(err)
	}

	projects, err := webapp.OpenProjectDB(filepath.Join(metaDir, "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	proj, _, err := projects.GetOrCreate("large-proj", org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := projects.SetFolder(proj.ID, webapp.FolderRule{
		Prefix:  "secret/",
		Default: webapp.PermNone,
	}); err != nil {
		t.Fatal(err)
	}

	projDir := filepath.Join(root, proj.ID)
	for _, sub := range []string{"journal", "blobs"} {
		if err := os.MkdirAll(filepath.Join(projDir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Deterministic content pool (5 blobs + empty blob) so blobs dir stays tiny.
	contents := map[string]string{
		"empty": "",
		"v0":    "version zero content\n",
		"v1":    "version one content\n",
		"v2":    "version two content\n",
		"sec":   "classified content\n",
	}
	shas := make(map[string]string, len(contents))
	for k, body := range contents {
		h := sha256HexStr(body)
		shas[k] = h
		if err := os.WriteFile(filepath.Join(projDir, "blobs", h), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var bigOps []journal.Op
	var seq int64
	var lam int64
	addBig := func(kind, path, contentKey string) {
		seq++
		lam++
		op := journal.Op{
			Seq:      seq,
			Lamport:  lam,
			Time:     t0.Add(time.Duration(lam) * time.Second),
			Device:   "dev-big",
			User:     "alice@example.com",
			UserName: "Alice",
			Kind:     kind,
			Path:     path,
		}
		if kind == journal.KindPut {
			op.Blob = shas[contentKey]
			op.Size = int64(len(contents[contentKey]))
			op.Mode = 0o644
		}
		bigOps = append(bigOps, op)
	}

	// Seed specific fixtures at known paths first.
	addBig(journal.KindPut, "public/doc-000.md", "v1")    // will be emptied below
	addBig(journal.KindPut, "public/doc-000.md", "empty") // emptied candidate
	addBig(journal.KindPut, "public/doc-001.md", "v1")    // upload If-Match target
	addBig(journal.KindPut, "public/doc-002.md", "v0")
	addBig(journal.KindPut, "public/doc-002.md", "v1") // history ?path= target
	addBig(journal.KindPut, "public/doc-003.md", "v0") // restore target (to v0)
	addBig(journal.KindPut, "public/doc-003.md", "v1")
	addBig(journal.KindPut, "public/doc-004.md", "v0") // undo-run base
	addBig(journal.KindPut, "secret/hidden.md", "sec")

	// Fill remaining ops across 200 distinct paths so totalOps = len(bigOps) + 3.
	const numPaths = 200
	for len(bigOps) < totalOps-3 {
		idx := (len(bigOps) % (numPaths - 5)) + 5
		p := fmt.Sprintf("public/doc-%03d.md", idx)
		key := "v1"
		if len(bigOps)%2 == 0 {
			key = "v2"
		}
		addBig(journal.KindPut, p, key)
	}

	rawBig, err := journal.Marshal(bigOps)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "journal", "dev-big.jsonl"), rawBig, 0o644); err != nil {
		t.Fatal(err)
	}

	// dev-run: 2 ops in session "run-1" modifying public/doc-004.md (so undo-run writes 1 restore op).
	runOps := []journal.Op{
		{
			Seq: 1, Lamport: lam + 1, Time: t0.Add(time.Duration(lam+1) * time.Second),
			Device: "dev-run", User: "alice@example.com", Kind: journal.KindPut,
			Path: "public/doc-004.md", Blob: shas["v1"], Size: int64(len(contents["v1"])), Mode: 0o644,
			Session: "run-1",
		},
		{
			Seq: 2, Lamport: lam + 2, Time: t0.Add(time.Duration(lam+2) * time.Second),
			Device: "dev-run", User: "alice@example.com", Kind: journal.KindPut,
			Path: "public/doc-004.md", Blob: shas["v2"], Size: int64(len(contents["v2"])), Mode: 0o644,
			Session: "run-1",
		},
	}
	rawRun, err := journal.Marshal(runOps)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "journal", "dev-run.jsonl"), rawRun, 0o644); err != nil {
		t.Fatal(err)
	}

	// Pre-seed hub's own journal with 1 op so incremental fold stays valid on hub writes.
	hubOps := []journal.Op{
		{
			Seq: 1, Lamport: lam + 3, Time: t0.Add(time.Duration(lam+3) * time.Second),
			Device: "hub-reg", User: "alice@example.com", Kind: journal.KindPut,
			Path: "public/hub-init.md", Blob: shas["v0"], Size: int64(len(contents["v0"])), Mode: 0o644,
		},
	}
	rawHub, err := journal.Marshal(hubOps)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "journal", "hub-reg.jsonl"), rawHub, 0o644); err != nil {
		t.Fatal(err)
	}

	srv := &webapp.Server{
		Root:     be,
		Projects: projects,
		Dir:      webapp.LocalDirectory{OrgDB: orgs},
		Auth:     &headerAuth{},
		Device:   webapp.Identity{ID: "hub-reg", Name: "hub", Author: "hub"},
		Upload:   webapp.UploadConfig{Enabled: true},
	}
	h := srv.Handler()

	// Prime jcache + initial fold + wait for async fold checkpoint (if >= 1 MiB) so
	// background goroutines do not pollute later TotalAlloc measurements.
	rec := doReq(h, "GET", "/api/p/"+proj.ID+"/tree?slim=1", "alice@example.com", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("prime tree: %d %s", rec.Code, rec.Body.String())
	}
	if len(rawBig) >= 1<<20 {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if ok, _ := be.Exists(ctx, proj.ID+"/hub-cache/fold-v1.bin.gz"); ok {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(30 * time.Millisecond)
	}

	return regressionFixture{
		handler:   h,
		be:        be,
		projDir:   projDir,
		projectID: proj.ID,
		shas:      shas,
	}
}

func doReq(h http.Handler, method, url, user string, headers map[string]string, body []byte) *httptest.ResponseRecorder {
	var br *bytes.Reader
	if body != nil {
		br = bytes.NewReader(body)
	} else {
		br = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, url, br)
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func measureTotalAlloc(t *testing.T, fn func()) uint64 {
	t.Helper()
	runtime.GC()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func runAllOpCopyProbes(t *testing.T, totalOps int, maxAlloc map[string]uint64, assertLimit bool) {
	t.Helper()
	fix := setupRegressionHub(t, totalOps)
	base := "/api/p/" + fix.projectID

	check := func(name string, ceiling uint64, fn func()) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			alloc := measureTotalAlloc(t, fn)
			t.Logf("ops=%d path=%s TotalAlloc=%.2f MiB (%d B), ceiling=%.2f MiB",
				totalOps, name, float64(alloc)/(1<<20), alloc, float64(ceiling)/(1<<20))
			if assertLimit && alloc > ceiling {
				t.Fatalf("%s allocated %.2f MiB (%d B), exceeds ceiling %.2f MiB (%d B)",
					name, float64(alloc)/(1<<20), alloc, float64(ceiling)/(1<<20), ceiling)
			}
		})
	}

	// 1. PUT /upload/content with If-Match (commitBase.holds + stamp + keepAfter)
	check("upload_if_match", maxAlloc["upload_if_match"], func() {
		rec := doReq(fix.handler, http.MethodPut, base+"/upload/content?path=public/doc-001.md",
			"alice@example.com", map[string]string{"If-Match": fix.shas["v1"]}, []byte("updated by upload\n"))
		if rec.Code != http.StatusOK {
			t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
		}
	})

	// 2. GET /history?path=... (cold histIndex + moveIndex)
	check("history_path", maxAlloc["history_path"], func() {
		rec := doReq(fix.handler, http.MethodGet, base+"/history?path=public/doc-002.md&n=10",
			"alice@example.com", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("history: %d %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Entries []webapp.HistoryEntry `json:"entries"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Entries) != 2 {
			t.Fatalf("history entries=%d err=%v body=%s", len(out.Entries), err, rec.Body.String())
		}
	})

	// 3. GET /history/emptied
	check("history_emptied", maxAlloc["history_emptied"], func() {
		rec := doReq(fix.handler, http.MethodGet, base+"/history/emptied",
			"alice@example.com", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("emptied: %d %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Paths []string `json:"paths"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Paths) != 1 || out.Paths[0] != "public/doc-000.md" {
			t.Fatalf("emptied paths=%v err=%v", out.Paths, err)
		}
	})

	// 4. POST /restore
	check("restore", maxAlloc["restore"], func() {
		body, _ := json.Marshal(map[string]string{"path": "public/doc-003.md", "sha": fix.shas["v0"]})
		rec := doReq(fix.handler, http.MethodPost, base+"/restore",
			"alice@example.com", nil, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("restore: %d %s", rec.Code, rec.Body.String())
		}
	})

	// 5. POST /undo-run
	check("undo_run", maxAlloc["undo_run"], func() {
		body, _ := json.Marshal(map[string]any{"device": "dev-run", "session": "run-1", "preview": false})
		rec := doReq(fix.handler, http.MethodPost, base+"/undo-run",
			"alice@example.com", nil, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("undo-run: %d %s", rec.Code, rec.Body.String())
		}
	})

	// 6. Restricted GET /blob (visibleSHASet)
	check("visible_sha_blob", maxAlloc["visible_sha_blob"], func() {
		rec := doReq(fix.handler, http.MethodGet, base+"/blob?sha="+fix.shas["v1"],
			"bob@example.com", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("blob: %d %s", rec.Code, rec.Body.String())
		}
		// Ensure hidden secret SHA is denied (404) under the same filter.
		hidden := doReq(fix.handler, http.MethodGet, base+"/blob?sha="+fix.shas["sec"],
			"bob@example.com", nil, nil)
		if hidden.Code != http.StatusNotFound {
			t.Fatalf("hidden blob status = %d, want 404", hidden.Code)
		}
	})

	// 7. GET /tree full refold (new tiny peer journal with new path forces full refold, jcache warm on dev-big)
	newOp := journal.Op{
		Seq: 1, Lamport: int64(totalOps) + 100, Time: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
		Device: "dev-new", User: "alice@example.com", Kind: journal.KindPut,
		Path: "public/new-trigger.md", Blob: fix.shas["v1"], Size: 20, Mode: 0o644,
	}
	rawNew, err := journal.Marshal([]journal.Op{newOp})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fix.projDir, "journal", "dev-new.jsonl"), rawNew, 0o644); err != nil {
		t.Fatal(err)
	}
	check("tree_refold", maxAlloc["tree_refold"], func() {
		rec := doReq(fix.handler, http.MethodGet, base+"/tree?slim=1",
			"alice@example.com", nil, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "new-trigger.md") {
			t.Fatalf("tree refold: %d %s", rec.Code, rec.Body.String())
		}
	})
}

var zeroCopyAllocCeilings = map[string]uint64{
	"upload_if_match":  3 << 20, // baseline ~14+ MiB
	"history_path":     5 << 20, // baseline ~24 MiB
	"history_emptied":  3 << 20, // baseline ~14+ MiB
	"restore":          4 << 20, // baseline ~28+ MiB
	"undo_run":         4 << 20, // baseline ~35+ MiB
	"visible_sha_blob": 2 << 20, // baseline ~9 MiB
	"tree_refold":      5 << 20, // baseline ~10+ MiB
}

func TestZeroCopyAllocRegression(t *testing.T) {
	runAllOpCopyProbes(t, 25000, zeroCopyAllocCeilings, true)
}

func TestZeroCopyHealthyControl(t *testing.T) {
	t.Run("small_project_100_ops", func(t *testing.T) {
		runAllOpCopyProbes(t, 100, zeroCopyAllocCeilings, true)
	})

	t.Run("warm_cache_hits_25000_ops", func(t *testing.T) {
		fix := setupRegressionHub(t, 25000)
		base := "/api/p/" + fix.projectID
		// Prime histIndex once.
		if rec := doReq(fix.handler, http.MethodGet, base+"/history?n=5", "alice@example.com", nil, nil); rec.Code != http.StatusOK {
			t.Fatalf("prime history: %d", rec.Code)
		}
		treeAlloc := measureTotalAlloc(t, func() {
			if rec := doReq(fix.handler, http.MethodGet, base+"/tree?slim=1", "alice@example.com", nil, nil); rec.Code != http.StatusOK {
				t.Fatalf("warm tree: %d", rec.Code)
			}
		})
		histAlloc := measureTotalAlloc(t, func() {
			if rec := doReq(fix.handler, http.MethodGet, base+"/history?n=5", "alice@example.com", nil, nil); rec.Code != http.StatusOK {
				t.Fatalf("warm history: %d", rec.Code)
			}
		})
		t.Logf("warm cache hits (25k ops): tree=%.2f MiB, history=%.2f MiB",
			float64(treeAlloc)/(1<<20), float64(histAlloc)/(1<<20))
		if treeAlloc > 1<<20 {
			t.Fatalf("warm tree allocated %d B > 1 MiB", treeAlloc)
		}
		if histAlloc > 1<<20 {
			t.Fatalf("warm history allocated %d B > 1 MiB", histAlloc)
		}
	})
}
