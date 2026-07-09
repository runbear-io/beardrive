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

// doAs is do() with a session cookie.
func doAs(t *testing.T, h http.Handler, method, url string, body any, c *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, url, rd)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestOrgDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orgs.json")
	db, err := OpenOrgDB(path)
	if err != nil {
		t.Fatal(err)
	}
	o, err := db.Create("acme", "Alice@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if db.Role(o.ID, "alice@example.com") != RoleOwner {
		t.Fatal("creator must be owner (email case-insensitive)")
	}
	if err := db.AddMember(o.ID, "bob@example.com", RoleMember); err != nil {
		t.Fatal(err)
	}
	// an invite never downgrades an owner
	if err := db.AddMember(o.ID, "alice@example.com", RoleMember); err != nil {
		t.Fatal(err)
	}
	if db.Role(o.ID, "alice@example.com") != RoleOwner {
		t.Fatal("AddMember downgraded the owner")
	}

	inv, err := db.CreateInvite(o.ID, "alice@example.com", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := db.Redeem(inv.Token); !ok || got.Org != o.ID {
		t.Fatal("live invite must redeem")
	}
	// expired invites don't
	old, _ := db.CreateInvite(o.ID, "alice@example.com", time.Nanosecond)
	time.Sleep(time.Millisecond)
	if _, ok := db.Redeem(old.Token); ok {
		t.Fatal("expired invite redeemed")
	}

	// everything persists across a reopen
	db2, err := OpenOrgDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if db2.Role(o.ID, "bob@example.com") != RoleMember {
		t.Fatal("membership lost on reload")
	}
	if _, ok := db2.Redeem(inv.Token); !ok {
		t.Fatal("invite lost on reload")
	}
}

// A pre-org hub must keep working after upgrade: every project lands in a
// default org, every existing account keeps access, oldest account owns it.
func TestMigrateOrgs(t *testing.T) {
	dir := t.TempDir()
	projects, _ := OpenProjectDB(filepath.Join(dir, "projects.json"))
	p1, _, _ := projects.GetOrCreate("one", "")
	p2, _, _ := projects.GetOrCreate("two", "")
	orgs, _ := OpenOrgDB(filepath.Join(dir, "orgs.json"))

	accounts := []User{
		{ID: "u-1", Email: "old@x.io"},
		{ID: "u-2", Email: "new@x.io"},
	}
	if err := MigrateOrgs(projects, orgs, accounts); err != nil {
		t.Fatal(err)
	}
	def := orgs.OrgsFor("old@x.io")
	if len(def) != 1 || def[0].Name != "default" {
		t.Fatalf("oldest account's orgs = %+v", def)
	}
	if orgs.Role(def[0].ID, "old@x.io") != RoleOwner || orgs.Role(def[0].ID, "new@x.io") != RoleMember {
		t.Fatal("roles: oldest must own, the rest join as members")
	}
	for _, id := range []string{p1.ID, p2.ID} {
		if p, _ := projects.Get(id); p.Org != def[0].ID {
			t.Fatalf("project %s not migrated: %+v", id, p)
		}
	}
	// idempotent: nothing to do the second time
	if err := MigrateOrgs(projects, orgs, accounts); err != nil {
		t.Fatal(err)
	}
	if got := orgs.OrgsFor("old@x.io"); len(got) != 1 {
		t.Fatalf("second migration created another org: %+v", got)
	}
}

// orgHub builds an auth+org hub with two accounts in two separate orgs,
// alice owning a project in hers.
func orgHub(t *testing.T) (h http.Handler, alice, bob *http.Cookie, pa Project) {
	t.Helper()
	srv, _, _ := newHub(t, true, nil)
	auth, err := OpenBuiltinAuth(filepath.Join(t.TempDir(), "auth.json"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Auth = auth
	orgs, err := OpenOrgDB(filepath.Join(t.TempDir(), "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Orgs = orgs
	shares, err := OpenShareDB(filepath.Join(t.TempDir(), "shares.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Shares = shares
	h = srv.Handler()

	alice = signupAndSession(t, h, "alice@x.io", "Alice", "password1")
	bob = signupAndSession(t, h, "bob@x.io", "Bob", "password1")

	// alice creates a project; with no org yet, she gets one of her own
	rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": "wiki"}, alice)
	if rec.Code != 200 {
		t.Fatalf("create project: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Project.Org == "" {
		t.Fatal("project created without an org")
	}
	return h, alice, bob, out.Project
}

// Every per-project route must reject a non-member and admit a member.
func TestOrgWallsProjectRoutes(t *testing.T) {
	h, alice, bob, pa := orgHub(t)
	base := "/api/p/" + pa.ID + "/"

	routes := []struct {
		method, url string
		body        any
	}{
		{"GET", base + "tree", nil},
		{"GET", base + "file?path=x.md", nil},
		{"GET", base + "download?path=x.md", nil},
		{"GET", base + "render?path=x.md", nil},
		{"GET", base + "history", nil},
		{"GET", base + "blob?sha=" + strings.Repeat("a", 64), nil},
		{"GET", base + "shares", nil},
		{"POST", base + "shares", map[string]string{"path": "x.md"}},
		{"POST", base + "upload/init", map[string]any{"path": "x.md", "sha256": strings.Repeat("a", 64), "size": 1}},
		{"PUT", base + "upload/content?path=x.md", []byte("hi")},
		{"POST", base + "upload/commit", map[string]any{"path": "x.md", "sha256": strings.Repeat("a", 64), "size": 1}},
		{"GET", base + "store/list?prefix=journal/", nil},
		{"GET", base + "store/object?key=journal/d.jsonl", nil},
		{"GET", base + "store/exists?key=journal/d.jsonl", nil},
		{"POST", base + "store/sign", map[string]any{"key": "blobs/" + strings.Repeat("a", 64), "size": 1}},
		{"PUT", base + "store/object?key=journal/d.jsonl", []byte("{}")},
	}
	for _, rt := range routes {
		if rec := doAs(t, h, rt.method, rt.url, rt.body, bob); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as non-member: %d, want 403", rt.method, rt.url, rec.Code)
		}
		if rec := doAs(t, h, rt.method, rt.url, rt.body, alice); rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
			t.Errorf("%s %s as member: %d, want access", rt.method, rt.url, rec.Code)
		}
	}

	// the project list shows it only to members; direct get 404s for bob
	if rec := doAs(t, h, "GET", "/api/projects", nil, bob); strings.Contains(rec.Body.String(), pa.ID) {
		t.Error("non-member sees the project in the list")
	}
	if rec := doAs(t, h, "GET", "/api/projects", nil, alice); !strings.Contains(rec.Body.String(), pa.ID) {
		t.Error("member does not see the project in the list")
	}
	if rec := doAs(t, h, "GET", "/api/projects/"+pa.ID, nil, bob); rec.Code != http.StatusNotFound {
		t.Errorf("non-member project get: %d, want 404", rec.Code)
	}
}

// An invite is minted by an owner, rejected for non-owners, and joining
// opens the wall.
func TestOrgInviteFlow(t *testing.T) {
	h, alice, bob, pa := orgHub(t)

	// only the owner can mint
	if rec := doAs(t, h, "POST", "/api/orgs/"+pa.Org+"/invites", nil, bob); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner minted an invite: %d", rec.Code)
	}
	rec := doAs(t, h, "POST", "/api/orgs/"+pa.Org+"/invites", nil, alice)
	if rec.Code != 200 {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body)
	}
	var inv struct {
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &inv); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inv.URL, "/#join/"+inv.Token) {
		t.Fatalf("invite URL = %q", inv.URL)
	}

	// bob joins, and the project opens up
	if rec := doAs(t, h, "POST", "/api/invites/"+inv.Token, nil, bob); rec.Code != 200 {
		t.Fatalf("accept: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "GET", "/api/p/"+pa.ID+"/tree", nil, bob); rec.Code != 200 {
		t.Fatalf("tree after joining: %d %s", rec.Code, rec.Body)
	}

	// a bogus token doesn't
	if rec := doAs(t, h, "POST", "/api/invites/deadbeef", nil, bob); rec.Code != http.StatusNotFound {
		t.Fatalf("bogus invite: %d", rec.Code)
	}
}

// Two orgs can each have a project with the same name.
func TestProjectNamesScopedToOrg(t *testing.T) {
	h, alice, bob, pa := orgHub(t)
	rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": pa.Name}, bob)
	if rec.Code != 200 {
		t.Fatalf("bob create: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
		Created bool    `json:"created"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.Created || out.Project.ID == pa.ID {
		t.Fatalf("bob must get his own %q, got %+v", pa.Name, out)
	}
	// while alice re-joins hers
	rec = doAs(t, h, "POST", "/api/projects", map[string]string{"name": pa.Name}, alice)
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Created || out.Project.ID != pa.ID {
		t.Fatalf("alice must re-join %s, got %+v", pa.ID, out)
	}
	_ = alice
}
