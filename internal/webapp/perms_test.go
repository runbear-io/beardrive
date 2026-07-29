package webapp

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestPermRankAndAtLeast(t *testing.T) {
	// An unknown level must fail closed — it is the answer for a corrupt
	// grant, and reading it as anything but "none" would open a hole.
	for _, l := range []string{"", "none", "bogus", "Admin"} {
		if permRank(l) != 0 {
			t.Errorf("permRank(%q) = %d, want 0", l, permRank(l))
		}
	}
	if !(permRank(PermRead) < permRank(PermWrite) && permRank(PermWrite) < permRank(PermAdmin)) {
		t.Fatal("levels are not ordered read < write < admin")
	}
	if !atLeast(PermAdmin, PermWrite) || !atLeast(PermWrite, PermWrite) || atLeast(PermRead, PermWrite) {
		t.Fatal("atLeast is wrong")
	}
}

// permHub builds an org hub where alice owns the org, bob and carol are plain
// members, and dave is in another org entirely. The project is alice's.
func permHub(t *testing.T) (h http.Handler, srv *Server, cookies map[string]*http.Cookie, p Project) {
	t.Helper()
	srv, _, _ = newHub(t, true, nil)
	auth, err := OpenBuiltinAuth(filepath.Join(t.TempDir(), "auth.json"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Auth = auth
	orgs, err := OpenOrgDB(filepath.Join(t.TempDir(), "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Dir = LocalDirectory{OrgDB: orgs}
	shares, err := OpenShareDB(filepath.Join(t.TempDir(), "shares.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Shares = shares
	h = srv.Handler()

	cookies = map[string]*http.Cookie{}
	for _, who := range []string{"alice", "bob", "carol", "dave"} {
		cookies[who] = signupAndSession(t, h, who+"@x.io", strings.ToUpper(who[:1])+who[1:], "password1")
	}

	rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": "wiki"}, cookies["alice"])
	if rec.Code != 200 {
		t.Fatalf("create project: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	p = out.Project
	for _, who := range []string{"bob", "carol"} {
		if err := orgs.AddMember(p.Org, who+"@x.io", RoleMember); err != nil {
			t.Fatal(err)
		}
	}
	return h, srv, cookies, p
}

// Nothing changes for an existing hub: with no permission edits, every org
// member still has full read+write on every project.
func TestDefaultIsWriteForEveryMember(t *testing.T) {
	h, _, c, p := permHub(t)
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["bob"]); rec.Code != 200 {
		t.Fatalf("member read: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/store/object?key=journal/d.jsonl", []byte("{}"), c["bob"]); rec.Code == http.StatusForbidden {
		t.Fatalf("member write refused by default: %s", rec.Body)
	}
	// and an outsider is still walled out
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["dave"]); rec.Code != http.StatusForbidden {
		t.Fatalf("outsider read: %d, want 403", rec.Code)
	}
}

// The creator of a project becomes its first admin — unless they are an org
// owner, who is implicitly admin and needs no grant.
func TestCreatorBecomesAdmin(t *testing.T) {
	h, srv, c, p := permHub(t)
	// alice created it as an org owner: implicit admin, no explicit grant.
	if got, _ := srv.Projects.Get(p.ID); got.Creator != "alice@x.io" {
		t.Fatalf("creator = %q, want alice@x.io", got.Creator)
	}
	// bob, a plain member, creates one: he gets the explicit admin grant.
	rec := doAs(t, h, "POST", "/api/projects", map[string]any{"name": "bobs", "org": p.Org}, c["bob"])
	if rec.Code != 200 {
		t.Fatalf("bob create: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Project map[string]any `json:"project"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Project["perm"] != PermAdmin {
		t.Fatalf("creator's own level = %v, want admin", out.Project["perm"])
	}
	bp, _ := srv.Projects.Get(out.Project["id"].(string))
	if bp.Perms["bob@x.io"] != PermAdmin {
		t.Fatalf("creator grant = %+v", bp.Perms)
	}
	// and a plain member who is a project admin can rename and delete it —
	// this used to be org-owner-only.
	id := out.Project["id"].(string)
	if rec := doAs(t, h, "PATCH", "/api/projects/"+id, map[string]string{"name": "bobs2"}, c["bob"]); rec.Code != 200 {
		t.Fatalf("project admin rename: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "DELETE", "/api/projects/"+id, nil, c["bob"]); rec.Code != 200 {
		t.Fatalf("project admin delete: %d %s", rec.Code, rec.Body)
	}
}

// A read grant admits every read route and refuses every write route.
func TestReadOnlyMemberRoutes(t *testing.T) {
	h, srv, c, p := permHub(t)
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	base := "/api/p/" + p.ID + "/"
	// A live link to re-date: PATCH /api/shares/{token} resolves the token
	// before the level check, so an unknown one would 404 instead of 403.
	sh, err := srv.Shares.Create(p.ID, "x.md", "alice@x.io", 0)
	if err != nil {
		t.Fatal(err)
	}
	writes := []struct {
		method, url string
		body        any
	}{
		{"PATCH", "/api/shares/" + sh.Token, map[string]string{"expires_in": "24h"}},
		{"POST", base + "upload/init", map[string]any{"path": "x.md", "sha256": strings.Repeat("a", 64), "size": 1}},
		{"PUT", base + "upload/content?path=x.md", []byte("hi")},
		{"POST", base + "upload/commit", map[string]any{"path": "x.md", "sha256": strings.Repeat("a", 64), "size": 1}},
		{"PUT", base + "store/object?key=journal/d.jsonl", []byte("{}")},
		{"POST", base + "store/sign", map[string]any{"key": "blobs/" + strings.Repeat("a", 64), "size": 1}},
		{"POST", base + "shares", map[string]string{"path": "x.md"}},
		{"PATCH", "/api/projects/" + p.ID, map[string]string{"name": "nope"}},
		{"DELETE", "/api/projects/" + p.ID, nil},
		{"PUT", base + "permissions", map[string]string{"default": "read"}},
		{"PUT", base + "permissions/carol@x.io", map[string]string{"level": "read"}},
		{"DELETE", base + "permissions/carol@x.io", nil},
	}
	for _, rt := range writes {
		if rec := doAs(t, h, rt.method, rt.url, rt.body, c["bob"]); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as read-only: %d, want 403", rt.method, rt.url, rec.Code)
		}
	}
	reads := []struct{ method, url string }{
		{"GET", base + "tree"},
		{"GET", base + "file?path=x.md"},
		{"GET", base + "download?path=x.md"},
		{"GET", base + "render?path=x.md"},
		{"GET", base + "history"},
		{"GET", base + "blob?sha=" + strings.Repeat("a", 64)},
		{"GET", base + "heat"},
		{"GET", base + "shares"},
		{"GET", base + "store/list?prefix=journal/"},
		{"GET", base + "store/object?key=journal/d.jsonl"},
		{"GET", base + "store/exists?key=journal/d.jsonl"},
		{"GET", base + "permissions"},
	}
	for _, rt := range reads {
		if rec := doAs(t, h, rt.method, rt.url, nil, c["bob"]); rec.Code == http.StatusForbidden {
			t.Errorf("%s %s as read-only: 403, want access (%s)", rt.method, rt.url, rec.Body)
		}
	}
	if rec := doAs(t, h, "POST", base+"reads", map[string]any{"reads": []any{}}, c["bob"]); rec.Code == http.StatusForbidden {
		t.Errorf("read report as read-only: 403, want access")
	}
	// a read member still sees the project and can open it
	if rec := doAs(t, h, "GET", "/api/projects", nil, c["bob"]); !strings.Contains(rec.Body.String(), p.ID) {
		t.Error("read-only member does not see the project in the list")
	}
}

// A none grant is treated exactly like a non-member: hidden from the list,
// 403 everywhere.
func TestNoAccessMemberIsInvisible(t *testing.T) {
	h, srv, c, p := permHub(t)
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermNone); err != nil {
		t.Fatal(err)
	}
	base := "/api/p/" + p.ID + "/"
	for _, url := range []string{"tree", "history", "heat", "shares", "permissions", "store/list?prefix=journal/"} {
		if rec := doAs(t, h, "GET", base+url, nil, c["bob"]); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s as none: %d, want 403", url, rec.Code)
		}
	}
	if rec := doAs(t, h, "GET", "/api/projects", nil, c["bob"]); strings.Contains(rec.Body.String(), p.ID) {
		t.Error("a none member sees the project in the list")
	}
	// create-or-join by name must not hand the id back either
	if rec := doAs(t, h, "POST", "/api/projects", map[string]any{"name": p.Name, "org": p.Org}, c["bob"]); rec.Code != http.StatusForbidden {
		t.Errorf("join-by-name as none: %d, want 403", rec.Code)
	}
	// carol, with no explicit grant, is unaffected
	if rec := doAs(t, h, "GET", base+"tree", nil, c["carol"]); rec.Code != 200 {
		t.Errorf("carol: %d %s", rec.Code, rec.Body)
	}
}

// Default none makes a project invite-only: only explicit grants and org
// owners get in.
func TestInviteOnlyDefault(t *testing.T) {
	h, srv, c, p := permHub(t)
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions", map[string]string{"default": "none"}, c["alice"]); rec.Code != 200 {
		t.Fatalf("set default: %d %s", rec.Code, rec.Body)
	}
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	base := "/api/p/" + p.ID + "/tree"
	if rec := doAs(t, h, "GET", base, nil, c["carol"]); rec.Code != http.StatusForbidden {
		t.Errorf("carol with default none: %d, want 403", rec.Code)
	}
	if rec := doAs(t, h, "GET", base, nil, c["bob"]); rec.Code != 200 {
		t.Errorf("bob with an explicit read grant: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "GET", base, nil, c["alice"]); rec.Code != 200 {
		t.Errorf("org owner locked out by default none: %d", rec.Code)
	}
	// admin is not a legal default
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions", map[string]string{"default": "admin"}, c["alice"]); rec.Code != http.StatusBadRequest {
		t.Errorf("default admin: %d, want 400", rec.Code)
	}
}

// An org owner always resolves to admin, whatever the grant list says, and a
// grant naming one is refused rather than silently ignored.
func TestOrgOwnerAlwaysAdmin(t *testing.T) {
	h, srv, c, p := permHub(t)
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermAdmin); err != nil {
		t.Fatal(err)
	}
	// bob (a project admin) tries to cut alice, the org owner, out
	rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/alice@x.io", map[string]string{"level": "none"}, c["bob"])
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("grant on an org owner: %d, want 400", rec.Code)
	}
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["alice"]); rec.Code != 200 {
		t.Fatalf("org owner locked out: %d", rec.Code)
	}
	// even a hand-written grant in storage cannot outrank her
	if err := srv.Projects.SetPerm(p.ID, "alice@x.io", PermNone); err != nil {
		t.Fatal(err)
	}
	if rec := doAs(t, h, "DELETE", "/api/projects/"+p.ID, nil, c["alice"]); rec.Code != 200 {
		t.Fatalf("org owner delete after a none grant: %d %s", rec.Code, rec.Body)
	}
}

// The last explicit admin cannot be removed or demoted — including by
// themselves.
func TestLastProjectAdminHeld(t *testing.T) {
	h, srv, c, p := permHub(t)
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermAdmin); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, url string
		body        any
	}{
		{"PUT", "/api/p/" + p.ID + "/permissions/bob@x.io", map[string]string{"level": "none"}},
		{"PUT", "/api/p/" + p.ID + "/permissions/bob@x.io", map[string]string{"level": "read"}},
		{"DELETE", "/api/p/" + p.ID + "/permissions/bob@x.io", nil},
	} {
		if rec := doAs(t, h, tc.method, tc.url, tc.body, c["bob"]); rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s: %d, want 400", tc.method, tc.url, rec.Code)
		}
		if got, _ := srv.Projects.Get(p.ID); got.Perms["bob@x.io"] != PermAdmin {
			t.Fatalf("last admin changed anyway: %+v", got.Perms)
		}
	}
	// with a second admin, the first can step down
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/carol@x.io", map[string]string{"level": "admin"}, c["bob"]); rec.Code != 200 {
		t.Fatalf("grant second admin: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "DELETE", "/api/p/"+p.ID+"/permissions/bob@x.io", nil, c["bob"]); rec.Code != 200 {
		t.Fatalf("step down with another admin present: %d %s", rec.Code, rec.Body)
	}
}

// Grants are org members only.
func TestGrantsAreOrgMembersOnly(t *testing.T) {
	h, _, c, p := permHub(t)
	rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/dave@x.io", map[string]string{"level": "read"}, c["alice"])
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("grant to a non-member: %d, want 400", rec.Code)
	}
	rec = doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/bob@x.io", map[string]string{"level": "bogus"}, c["alice"])
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown level: %d, want 400", rec.Code)
	}
}

// GET /permissions reports the default, the caller's own level, and grants.
func TestPermissionsGET(t *testing.T) {
	h, srv, c, p := permHub(t)
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/permissions", nil, c["bob"])
	if rec.Code != 200 {
		t.Fatalf("GET permissions as a read member: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Default string              `json:"default"`
		Me      string              `json:"me"`
		Grants  []map[string]string `json:"grants"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Default != PermWrite || out.Me != PermRead {
		t.Fatalf("default=%q me=%q, want write/read", out.Default, out.Me)
	}
	if len(out.Grants) != 1 || out.Grants[0]["email"] != "bob@x.io" {
		t.Fatalf("grants = %+v", out.Grants)
	}
}

// The project list carries the caller's level *alongside* every ordinary
// Project field. Regression guard: an earlier version hand-listed the fields
// it returned, which silently dropped description and icon the moment those
// were added — the client saw a project with no metadata and no error.
func TestProjectListCarriesWholeProject(t *testing.T) {
	h, srv, c, p := permHub(t)
	desc, icon := "everything support needs", "book-open"
	if err := srv.Projects.Update(p.ID, nil, &desc, &icon); err != nil {
		t.Fatal(err)
	}
	rec := doAs(t, h, "GET", "/api/projects", nil, c["alice"])
	var out struct {
		Projects []map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	for _, r := range out.Projects {
		if r["id"] == p.ID {
			row = r
		}
	}
	if row == nil {
		t.Fatalf("project missing from the list: %s", rec.Body)
	}
	for key, want := range map[string]any{
		"name": p.Name, "description": desc, "icon": icon, "perm": PermAdmin,
	} {
		if row[key] != want {
			t.Errorf("list row %q = %v, want %v", key, row[key], want)
		}
	}
	// The grant list is not list-response material — /permissions owns it.
	if _, leaked := row["perms"]; leaked {
		t.Errorf("grant list leaked into the project list: %v", row["perms"])
	}
}
