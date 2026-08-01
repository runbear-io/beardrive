package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Scoreboard row 11 — path handling. Everything a client can steer a read or
// a write with: ?path=, ?prefix=, ?sha=, the JSON {path} of the write routes,
// and — the one nobody validates — the Blob field of a journal op, which is
// concatenated straight into a storage key.
//
// Plus the two route families round 1 never touched: the single-volume
// (DirSource) viewer/upload endpoints behind the `single()` wrapper, and
// /auth/verify.

// ---- helpers (secpath* so they cannot collide with another agent's file) ----

// secpathDirServer serves a plain folder, optionally writable. dir_test.go's
// dirServer is read-only and hands back no root, and both are needed here.
func secpathDirServer(t *testing.T, root string, upload bool) http.Handler {
	t.Helper()
	return (&Server{
		Source: &DirSource{Root: root}, Volume: "local", Refresh: 0,
		Upload: UploadConfig{Enabled: upload},
	}).Handler()
}

// secpathWrite drops a file on disk, creating parents.
func secpathWrite(t *testing.T, abs, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// secpathPushOp writes a one-op journal for device dev into project id through
// the public store API — exactly what any device with write permission does on
// every sync. cookie may be nil on an auth-less hub.
func secpathPushOp(t *testing.T, h http.Handler, id, dev string, op map[string]any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	line, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("PUT",
		"/api/p/"+id+"/store/object?key=journal/"+dev+".jsonl",
		strings.NewReader(string(line)+"\n"))
	req.Header.Set("X-Bdrive-Device", dev)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func secpathPutOp(path, blob string, size int) map[string]any {
	return map[string]any{
		"seq": 1, "lamport": 1, "time": time.Now().UTC().Format(time.RFC3339Nano),
		"kind": "put", "path": path, "blob": blob, "size": size,
	}
}

// secpathNewProject creates a project by name and returns its id.
func secpathNewProject(t *testing.T, h http.Handler, name string, cookie *http.Cookie) Project {
	t.Helper()
	rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": name}, cookie)
	if rec.Code != 200 {
		t.Fatalf("create project %s: %d %s", name, rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Project
}

// secpathStoreBlob uploads content as a blob into a project's store.
func secpathStoreBlob(t *testing.T, h http.Handler, id, content string, cookie *http.Cookie) string {
	t.Helper()
	sha := shaOf(content)
	rec := doAs(t, h, "PUT", "/api/p/"+id+"/store/object?key=blobs/"+sha, []byte(content), cookie)
	if rec.Code != 200 {
		t.Fatalf("store blob in %s: %d %s", id, rec.Code, rec.Body)
	}
	return sha
}

// ---- row 11: the journal's Blob field is a storage key ----

// A journal op's Blob is concatenated into "blobs/"+Blob and handed to the
// backend (RemoteSource.Open), which filepath.Joins it under the storage root.
// Nothing between the wire and the filesystem checks that it is a sha256, so a
// device with write permission on ONE project can name any path relative to
// that project's prefix — another project's blob, or a file outside the whole
// storage root — and then read it back byte for byte through the ordinary
// viewer route.
//
// The control in each half proves the route works normally; the delta is the
// finding.
func TestSec_Path_ViewerBlobEscapesProjectPrefix(t *testing.T) {
	srv, victim, root := newHub(t, true, nil)
	h := srv.Handler()

	// A secret that has nothing to do with the hub's object store, one level
	// above the storage root — stand-in for anything else on the hub host.
	outside := filepath.Join(filepath.Dir(root), "hub-secret.txt")
	secpathWrite(t, outside, "TOP-SECRET-OUTSIDE-ROOT")

	// A second project the attacker has no business reading.
	other := secpathNewProject(t, h, "other-project", nil)
	otherSHA := secpathStoreBlob(t, h, other.ID, "OTHER-PROJECT-CONTENT", nil)

	// Control: an honest op in the attacker's own project reads back fine.
	ownSHA := secpathStoreBlob(t, h, victim.ID, "my own file", nil)
	if rec := secpathPushOp(t, h, victim.ID, "attacker", secpathPutOp("mine.md", ownSHA, 11), nil); rec.Code != 200 {
		t.Fatalf("control journal push: %d %s", rec.Code, rec.Body)
	}
	if rec := get(t, h, "/api/p/"+victim.ID+"/file?path=mine.md"); rec.Code != 200 || rec.Body.String() != "my own file" {
		t.Fatalf("control read: %d %q", rec.Code, rec.Body)
	}

	for _, tc := range []struct {
		name, blob, secret string
	}{
		{"another project's blob", "../../" + other.ID + "/blobs/" + otherSHA, "OTHER-PROJECT-CONTENT"},
		{"a file outside the storage root", "../../../hub-secret.txt", "TOP-SECRET-OUTSIDE-ROOT"},
	} {
		p := "pwned-" + strings.ReplaceAll(tc.name, " ", "-") + ".txt"
		if rec := secpathPushOp(t, h, victim.ID, "attacker2", secpathPutOp(p, tc.blob, len(tc.secret)), nil); rec.Code != 200 {
			t.Fatalf("%s: journal push refused (%d %s) — attack setup failed", tc.name, rec.Code, rec.Body)
		}
		for _, route := range []string{"file", "download", "render"} {
			rec := get(t, h, "/api/p/"+victim.ID+"/"+route+"?path="+p)
			if strings.Contains(rec.Body.String(), tc.secret) {
				t.Errorf("/%s served %s via a traversing blob key (%d): %s",
					route, tc.name, rec.Code, rec.Body)
			}
		}
	}
}

// The same escape across an organization boundary, with real accounts: bob is
// a plain write member of alice's org and has never been near dave's org. He
// pushes one journal op into a project he legitimately writes and reads dave's
// file out of it.
func TestSec_Path_MemberReadsAnotherOrgsBlob(t *testing.T) {
	h, _, c, p := permHub(t)

	// dave is in his own org; his project is invisible to bob by every
	// documented route.
	daves := secpathNewProject(t, h, "daves-private", c["dave"])
	secret := "DAVES-CONFIDENTIAL-NOTES"
	sha := secpathStoreBlob(t, h, daves.ID, secret, c["dave"])

	// Control: the honest route is refused, as row 4 already asserts.
	if rec := doAs(t, h, "GET", "/api/p/"+daves.ID+"/blob?sha="+sha, nil, c["bob"]); rec.Code != http.StatusForbidden {
		t.Fatalf("control: bob reading dave's project directly = %d, want 403", rec.Code)
	}

	if rec := secpathPushOp(t, h, p.ID, "bobdev", secpathPutOp("loot.md", "../../"+daves.ID+"/blobs/"+sha, len(secret)), c["bob"]); rec.Code != 200 {
		t.Fatalf("bob's journal push: %d %s", rec.Code, rec.Body)
	}
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/file?path=loot.md", nil, c["bob"])
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("bob read another org's file through his own project (%d): %s", rec.Code, rec.Body)
	}
}

// ---- row 11: the ?sha= parameters ----

// Every route that takes a raw content hash must require exactly 64 lowercase
// hex chars — the hash is a storage key suffix, so anything else is a key the
// caller wrote.
func TestSec_Path_ShaParamsRejectNonHex(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	good := secpathStoreBlob(t, h, p.ID, "hello", nil)
	if rec := get(t, h, "/api/p/"+p.ID+"/blob?sha="+good); rec.Code != 200 {
		t.Fatalf("control blob read: %d %s", rec.Code, rec.Body)
	}

	bad := []string{
		"../../../../etc/passwd",
		"..%2f..%2fetc%2fpasswd",
		"..%252f..%252fpasswd",
		"..\\..\\windows\\win.ini",
		strings.Repeat("a", 64) + "/../../etc/passwd",
		strings.ToUpper(good),
		good[:63],
		good + "a",
		good + "%00.txt",
		"", // missing
	}
	for _, sha := range bad {
		for _, route := range []string{"blob", "render"} {
			rec := get(t, h, "/api/p/"+p.ID+"/"+route+"?sha="+sha+"&path=x.md")
			if rec.Code == 200 {
				t.Errorf("/%s?sha=%q returned 200: %s", route, sha, rec.Body)
			}
		}
	}
}

// A sha is only a key inside the caller's own project prefix: naming a blob
// that exists in a different project must miss.
func TestSec_Path_ShaFromAnotherProjectMisses(t *testing.T) {
	srv, mine, _ := newHub(t, true, nil)
	h := srv.Handler()
	other := secpathNewProject(t, h, "elsewhere", nil)
	sha := secpathStoreBlob(t, h, other.ID, "NOT-YOURS", nil)

	rec := get(t, h, "/api/p/"+mine.ID+"/blob?sha="+sha)
	if rec.Code == 200 || strings.Contains(rec.Body.String(), "NOT-YOURS") {
		t.Fatalf("blob crossed the project prefix: %d %s", rec.Code, rec.Body)
	}
}

// serveBlob sandboxes inline HTML and SVG so synced markup can never run with
// the hub origin's session (same posture as /s/*). handleBlob serves the exact
// same bytes for a historical version and takes the content type from a
// caller-supplied ?name= — so it must carry the same wall. Without it, any
// member who can write one .html file has script running on the hub origin,
// with the reader's session cookie, the moment a teammate opens that version.
func TestSec_Path_BlobInlineHTMLIsSandboxed(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	const evil = `<script>fetch('/api/projects')</script>`
	sha := secpathStoreBlob(t, h, p.ID, evil, nil)
	if rec := secpathPushOp(t, h, p.ID, "dev1", secpathPutOp("page.html", sha, len(evil)), nil); rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}

	// Control: the live-file route already walls it off.
	rec := get(t, h, "/api/p/"+p.ID+"/file?path=page.html")
	if rec.Code != 200 || rec.Header().Get("Content-Security-Policy") != "sandbox allow-scripts" {
		t.Fatalf("control /file: %d CSP=%q", rec.Code, rec.Header().Get("Content-Security-Policy"))
	}

	for _, name := range []string{"page.html", "page.htm", "drawing.svg", "page.xhtml"} {
		rec := get(t, h, "/api/p/"+p.ID+"/blob?sha="+sha+"&name="+name)
		if rec.Code != 200 {
			continue // this extension isn't served inline at all — fine
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.HasPrefix(ct, "text/html") && !strings.Contains(ct, "svg") && !strings.Contains(ct, "xhtml") {
			continue // served as something inert
		}
		if csp := rec.Header().Get("Content-Security-Policy"); csp != "sandbox allow-scripts" {
			t.Errorf("/blob?name=%s serves %s on the hub origin with CSP %q, want sandbox", name, ct, csp)
		}
	}
}

// ---- row 11: the write routes' {path} ----

// Every client-supplied destination path goes through cleanUploadPath. Prove
// the whole battery is refused on each route that journals a path, so no
// future caller can journal ../ or an absolute path (a peer materializes what
// the journal says).
func TestSec_Path_WriteRoutesRefuseTraversal(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	sha := secpathStoreBlob(t, h, p.ID, "payload", nil)

	// Control: an ordinary path commits.
	if rec := do(t, h, "POST", "/api/p/"+p.ID+"/upload/commit",
		map[string]any{"path": "ok.md", "sha256": sha, "size": 7}); rec.Code != 200 {
		t.Fatalf("control commit: %d %s", rec.Code, rec.Body)
	}

	evil := []string{
		"../escape.md",
		"a/../../escape.md",
		"/etc/passwd",
		"//etc/passwd",
		"./escape.md",
		"a//b.md",
		"a/./b.md",
		"..",
		"../",
		"notes/../../escape.md",
		"trailing/",
		".bdrive/config.json",
		"deep/.bdrive/config.json",
		".git/hooks/pre-commit",
		"",
	}
	for _, bad := range evil {
		if rec := do(t, h, "POST", "/api/p/"+p.ID+"/upload/init",
			map[string]any{"path": bad, "sha256": sha, "size": 7}); rec.Code != http.StatusBadRequest {
			t.Errorf("upload/init path=%q: %d, want 400: %s", bad, rec.Code, rec.Body)
		}
		if rec := do(t, h, "POST", "/api/p/"+p.ID+"/upload/commit",
			map[string]any{"path": bad, "sha256": sha, "size": 7}); rec.Code != http.StatusBadRequest {
			t.Errorf("upload/commit path=%q: %d, want 400: %s", bad, rec.Code, rec.Body)
		}
		if rec := do(t, h, "POST", "/api/p/"+p.ID+"/restore",
			map[string]any{"path": bad, "sha": sha}); rec.Code != http.StatusBadRequest {
			t.Errorf("restore path=%q: %d, want 400: %s", bad, rec.Code, rec.Body)
		}
		if rec := do(t, h, "POST", "/api/p/"+p.ID+"/remove",
			map[string]any{"path": bad}); rec.Code != http.StatusBadRequest {
			t.Errorf("remove path=%q: %d, want 400: %s", bad, rec.Code, rec.Body)
		}
	}
}

// restore pastes an existing blob onto a path. The sha must be a version of
// THAT path, or restore becomes "copy any blob in the store anywhere".
func TestSec_Path_RestoreRefusesForeignSHA(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	secret := "PRIVATE-VERSION"
	sha := secpathStoreBlob(t, h, p.ID, secret, nil)
	if rec := secpathPushOp(t, h, p.ID, "dev1", secpathPutOp("private.md", sha, len(secret)), nil); rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	rec := do(t, h, "POST", "/api/p/"+p.ID+"/restore", map[string]any{"path": "public.md", "sha": sha})
	if rec.Code == 200 {
		t.Fatalf("restore pasted another path's version onto public.md: %s", rec.Body)
	}
}

// ---- row 11: the single-volume (DirSource) viewer ----

// The plain-folder viewer resolves ?path= against a snapshot built by walking
// Root, so the map keys are the only reachable paths. Assert the whole
// traversal battery against every read route rather than trusting that.
func TestSec_Path_DirViewerRefusesTraversal(t *testing.T) {
	root := t.TempDir()
	secpathWrite(t, filepath.Join(root, "notes", "plan.md"), "inside")
	secpathWrite(t, filepath.Join(root, ".bdrive", "config.json"), `{"mount_id":"m","remote":"https://hub/p/x"}`)
	secpathWrite(t, filepath.Join(filepath.Dir(root), "outside.txt"), "OUTSIDE-THE-SERVED-FOLDER")
	h := secpathDirServer(t, root, false)

	if rec := get(t, h, "/api/file?path=notes/plan.md"); rec.Code != 200 || rec.Body.String() != "inside" {
		t.Fatalf("control read: %d %q", rec.Code, rec.Body)
	}

	evil := []string{
		"../outside.txt",
		"../../outside.txt",
		"notes/../../outside.txt",
		"..%2Foutside.txt",
		"..%252Foutside.txt",
		"%2e%2e/outside.txt",
		"..%5Coutside.txt",
		"..\\outside.txt",
		"/etc/hosts",
		"//etc/hosts",
		"/" + filepath.Join(filepath.Dir(root), "outside.txt"),
		filepath.Join(filepath.Dir(root), "outside.txt"),
		"notes/plan.md%00.png",
		"./notes/plan.md",
		"notes//plan.md",
		".bdrive/config.json",
		".bdrive",
		"..%c0%afoutside.txt", // overlong-UTF-8 slash
	}
	for _, bad := range evil {
		for _, route := range []string{"file", "download", "render"} {
			rec := get(t, h, "/api/"+route+"?path="+bad)
			if rec.Code == 200 {
				t.Errorf("/api/%s?path=%q returned 200: %.80q", route, bad, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "OUTSIDE-THE-SERVED-FOLDER") ||
				strings.Contains(rec.Body.String(), "mount_id") {
				t.Errorf("/api/%s?path=%q leaked content: %s", route, bad, rec.Body)
			}
		}
	}
	// The tree must not advertise .bdrive either.
	if body := get(t, h, "/api/tree").Body.String(); strings.Contains(body, ".bdrive") {
		t.Errorf("tree lists the settings dir: %s", body)
	}
}

// A symlink inside the served folder pointing outside it must not become a
// readable file: the viewer promises "this folder", and a link is not content.
func TestSec_Path_DirSymlinkIsNotServed(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "linked-secret.txt")
	secpathWrite(t, outside, "LINKED-SECRET")
	secpathWrite(t, filepath.Join(root, "notes", "plan.md"), "inside")
	if err := os.Symlink(outside, filepath.Join(root, "leak.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Dir(root), filepath.Join(root, "up")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	h := secpathDirServer(t, root, false)

	if body := get(t, h, "/api/tree").Body.String(); strings.Contains(body, "leak.txt") || strings.Contains(body, `"up"`) {
		t.Errorf("tree exposes symlinks out of the folder: %s", body)
	}
	for _, p := range []string{"leak.txt", "up/linked-secret.txt", "up/../linked-secret.txt"} {
		rec := get(t, h, "/api/file?path="+p)
		if rec.Code == 200 || strings.Contains(rec.Body.String(), "LINKED-SECRET") {
			t.Errorf("/api/file?path=%s served a symlinked file (%d): %s", p, rec.Code, rec.Body)
		}
	}
}

// The write half of the same question: an upload path with no ".." in it can
// still leave the served folder by walking through a symlinked directory,
// because DirSource.Upload joins and writes without checking where it landed.
func TestSec_Path_DirUploadCannotEscapeThroughSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir() // a directory that is NOT the served folder
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	h := secpathDirServer(t, root, true)

	// Control: an ordinary upload lands inside.
	rec := doAs(t, h, "PUT", "/api/upload/content?path=ok.txt", []byte("fine"), nil)
	if rec.Code != 200 {
		t.Fatalf("control upload: %d %s", rec.Code, rec.Body)
	}

	rec = doAs(t, h, "PUT", "/api/upload/content?path=link/pwned.txt", []byte("ESCAPED"), nil)
	if data, err := os.ReadFile(filepath.Join(outside, "pwned.txt")); err == nil {
		t.Fatalf("upload wrote outside the served folder (%d %s): %s at %s",
			rec.Code, rec.Body, data, filepath.Join(outside, "pwned.txt"))
	}
}

// ---- the single() wrapper: route families are mode-scoped ----

// single() ignores the declared permission level, which is only safe if those
// routes exist in exactly one mode. Prove both directions, and that a hub with
// auth still gates them (the wrapper runs inside authGate, not around it).
func TestSec_Path_SingleVolumeRoutesAreModeScoped(t *testing.T) {
	// Hub mode: the unprefixed viewer/upload family must not exist at all.
	srv, p, _ := newHub(t, true, nil)
	hub := srv.Handler()
	for _, u := range []string{
		"GET /api/tree", "GET /api/file?path=x", "GET /api/download?path=x", "GET /api/render?path=x",
	} {
		m, url, _ := strings.Cut(u, " ")
		if rec := do(t, hub, m, url, nil); rec.Code != http.StatusNotFound {
			t.Errorf("hub %s: %d, want 404: %s", u, rec.Code, rec.Body)
		}
	}
	for _, u := range []string{"POST /api/upload/init", "POST /api/upload/commit"} {
		m, url, _ := strings.Cut(u, " ")
		if rec := do(t, hub, m, url, map[string]any{"path": "x", "sha256": strings.Repeat("a", 64)}); rec.Code != http.StatusNotFound {
			t.Errorf("hub %s: %d, want 404: %s", u, rec.Code, rec.Body)
		}
	}
	if rec := do(t, hub, "PUT", "/api/upload/content?path=x", []byte("y")); rec.Code != http.StatusNotFound {
		t.Errorf("hub PUT /api/upload/content: %d, want 404: %s", rec.Code, rec.Body)
	}

	// Single-volume mode: no project routes, whatever id is named. (".." is
	// normalized away by the mux before routing, so it can only ever reach the
	// unprefixed family of this same server — assert it never serves content.)
	dir := secpathDirServer(t, t.TempDir(), true)
	for _, id := range []string{p.ID, "p-deadbeef", "..", "%2e%2e"} {
		if rec := get(t, dir, "/api/p/"+id+"/tree"); rec.Code == 200 {
			t.Errorf("volume-mode /api/p/%s/tree served content: %s", id, rec.Body)
		}
		if rec := get(t, dir, "/api/p/"+id+"/store/object?key=blobs/"+strings.Repeat("a", 64)); rec.Code == 200 {
			t.Errorf("volume-mode store on /api/p/%s served content: %s", id, rec.Body)
		}
	}
	for _, id := range []string{p.ID, "p-deadbeef"} {
		if rec := get(t, dir, "/api/p/"+id+"/tree"); rec.Code != http.StatusNotFound {
			t.Errorf("volume-mode /api/p/%s/tree: %d, want 404: %s", id, rec.Code, rec.Body)
		}
	}

	// An authenticated hub gates the family before the mode check: a stranger
	// gets 401, never a 404 that would confirm the shape of the server.
	authed, _, _, _ := permHub(t)
	for _, u := range []string{"/api/tree", "/api/file?path=x", "/api/render?path=x"} {
		if rec := get(t, authed, u); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s on an auth hub: %d, want 401: %s", u, rec.Code, rec.Body)
		}
	}
}

// ---- /auth/verify ----

// A verification link is a one-shot bearer of "this mailbox is real". It must
// be single-use, expiring, unguessable, and bound to its own grant kind — a
// password-reset token must never activate an account, and vice versa.
func TestSec_Path_VerifyGrantIsSingleUseAndTypeBound(t *testing.T) {
	a := gatedAuth(t, func(a *BuiltinAuth) { a.RequireVerification = true })
	h := (&Server{Auth: a}).Handler()

	u, err := a.signup("victim@x.io", "V", "password1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != statusUnverified {
		t.Fatalf("status = %q, want unverified", u.Status)
	}

	// Forged / empty / foreign tokens never activate anything.
	for _, tok := range []string{"", "deadbeef", strings.Repeat("0", 32), "../../x"} {
		rec := get(t, h, "/auth/verify?token="+tok)
		if strings.Contains(rec.Body.String(), "Email verified") {
			t.Errorf("forged verify token %q was accepted: %s", tok, rec.Body)
		}
	}
	a.mu.Lock()
	st := a.users[u.ID].Status
	a.mu.Unlock()
	if st != statusUnverified {
		t.Fatalf("account activated by a forged token: status = %q", st)
	}

	// A reset grant must not double as a verify grant.
	reset := a.newGrant("reset", u.ID, time.Hour)
	if rec := get(t, h, "/auth/verify?token="+reset); strings.Contains(rec.Body.String(), "Email verified") {
		t.Fatalf("a reset token verified an account: %s", rec.Body)
	}

	// The real link works exactly once.
	tok := a.newGrant("verify", u.ID, time.Hour)
	if rec := get(t, h, "/auth/verify?token="+tok); rec.Code != http.StatusSeeOther && !strings.Contains(rec.Body.String(), "verified") {
		t.Fatalf("valid verify link: %d %s", rec.Code, rec.Body)
	}
	a.mu.Lock()
	st = a.users[u.ID].Status
	a.mu.Unlock()
	if st == statusUnverified {
		t.Fatalf("valid verify link did not activate the account")
	}
	replay := get(t, h, "/auth/verify?token="+tok)
	if replay.Code == http.StatusSeeOther || !strings.Contains(replay.Body.String(), "expired") {
		t.Fatalf("verify token replayed: %d %s", replay.Code, replay.Body)
	}

	// An expired link is dead even on first use.
	expired := a.newGrant("verify", u.ID, -time.Second)
	if rec := get(t, h, "/auth/verify?token="+expired); !strings.Contains(rec.Body.String(), "expired") {
		t.Fatalf("expired verify link accepted: %d %s", rec.Code, rec.Body)
	}
}

// /auth/logout is a GET, so any page can trigger it cross-site. That is a
// nuisance, not a breach — but it must at least do its job: the revoked
// session token must be dead everywhere afterwards, not just cleared from the
// browser's cookie jar.
func TestSec_Path_LogoutRevokesTheTokenNotJustTheCookie(t *testing.T) {
	h, _, c, p := permHub(t)
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["alice"]); rec.Code != 200 {
		t.Fatalf("control: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "GET", "/auth/logout", nil, c["alice"]); rec.Code != http.StatusSeeOther {
		t.Fatalf("logout: %d %s", rec.Code, rec.Body)
	}
	// Replaying the same cookie value must now fail: a stolen session id has
	// to die with the sign-out, not linger until it expires.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["alice"]); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session survived logout: %d %s", rec.Code, rec.Body)
	}
}

// safeNext is the only thing keeping /auth/* redirects on this hub, and it
// tests one shape: a literal leading "//". A browser resolving a Location
// header treats a backslash as a path separator in the authority position and
// strips tab/newline before parsing, so "/\evil.example" and "/\t/evil.example"
// both land on evil.example — from the real hub's sign-in page, with the
// victim's credentials just typed in. Every /auth/* redirect must stay on this
// origin no matter what ?next= says.
func TestSec_Path_AuthNextCannotLeaveTheHub(t *testing.T) {
	a := gatedAuth(t, nil)
	h := (&Server{Auth: a}).Handler()
	if _, err := a.signup("victim@x.io", "V", "password1"); err != nil {
		t.Fatal(err)
	}

	offsite := []string{
		"//evil.example/",
		"https://evil.example/",
		"/\\evil.example/",
		"/\\/evil.example",
		"/\t/evil.example",
		"/\n/evil.example",
		"/\\\\evil.example",
	}
	for _, next := range offsite {
		// The post-sign-in redirect: the one that navigates a user who has
		// just proven they trust this page.
		form := url.Values{"email": {"victim@x.io"}, "password": {"password1"}}
		req := httptest.NewRequest("POST", "/auth/login?next="+url.QueryEscape(next),
			strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if loc := rec.Header().Get("Location"); secpathOffsite(loc) {
			t.Errorf("login ?next=%q redirected off-site: Location=%q", next, loc)
		}
		// ...and the logout redirect, which carries next through to login.
		rec = get(t, h, "/auth/logout?next="+url.QueryEscape(next))
		if loc := rec.Header().Get("Location"); secpathOffsite(loc) {
			t.Errorf("logout ?next=%q redirected off-site: Location=%q", next, loc)
		}
	}
}

// secpathOffsite reports whether a Location a browser follows can leave this
// origin: an absolute URL, or a path whose authority slot a browser would fill
// in for us (leading //, or / followed by a backslash or a stripped control
// character).
func secpathOffsite(loc string) bool {
	if loc == "" {
		return false
	}
	if u, err := url.Parse(loc); err == nil && (u.Scheme != "" || u.Host != "") {
		return true
	}
	// Browsers strip tab/CR/LF anywhere in a URL before parsing, and treat a
	// backslash as "/" in the authority position of a special scheme.
	stripped := strings.Map(func(r rune) rune {
		if r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, loc)
	stripped = strings.ReplaceAll(stripped, "\\", "/")
	return strings.HasPrefix(stripped, "//")
}
