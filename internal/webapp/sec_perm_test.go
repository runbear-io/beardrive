package webapp

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Security round 1 — scoreboard rows 2 (per-project permission model) and
// 4 (cross-org isolation). Every test here asserts the SECURE behavior, so a
// failure is the hole and a pass is the regression guard.
//
// Fixture is permHub (perms_test.go): alice owns the org and project p, bob
// and carol are plain org members, dave is in a different org entirely.

// secpermWrite is one PermWrite-level request shape, valid enough that an
// authorized caller gets something other than 403.
type secpermReq struct {
	method, url string
	body        any
}

// secpermWrites returns every PermWrite action against project id.
func secpermWrites(id, shareToken string) []secpermReq {
	base := "/api/p/" + id + "/"
	sha := strings.Repeat("a", 64)
	return []secpermReq{
		{"POST", base + "upload/init", map[string]any{"path": "x.md", "sha256": sha, "size": 1}},
		{"PUT", base + "upload/content?path=x.md", []byte("hi")},
		{"POST", base + "upload/commit", map[string]any{"path": "x.md", "sha256": sha, "size": 1}},
		{"PUT", base + "store/object?key=journal/d.jsonl", []byte("{}")},
		{"POST", base + "store/sign", map[string]any{"key": "blobs/" + sha, "size": 1}},
		{"POST", base + "shares", map[string]string{"path": "x.md"}},
		{"POST", base + "restore", map[string]string{"path": "x.md", "sha": sha}},
		{"POST", base + "remove", map[string]string{"path": "x.md"}},
		{"PATCH", "/api/shares/" + shareToken, map[string]string{"expires_in": "24h"}},
	}
}

// secpermAdmins returns every PermAdmin action against project id.
func secpermAdmins(id string) []secpermReq {
	base := "/api/p/" + id + "/"
	return []secpermReq{
		{"PATCH", "/api/projects/" + id, map[string]string{"name": "pwned"}},
		{"PUT", base + "permissions", map[string]string{"default": "read"}},
		{"PUT", base + "permissions/carol@x.io", map[string]string{"level": "admin"}},
		{"DELETE", base + "permissions/carol@x.io", nil},
		{"DELETE", "/api/projects/" + id, nil},
	}
}

// secpermReads returns every PermRead action against project id.
func secpermReads(id string) []secpermReq {
	base := "/api/p/" + id + "/"
	return []secpermReq{
		{"GET", base + "tree", nil},
		{"GET", base + "file?path=x.md", nil},
		{"GET", base + "download?path=x.md", nil},
		{"GET", base + "render?path=x.md", nil},
		{"GET", base + "history", nil},
		{"GET", base + "blob?sha=" + strings.Repeat("a", 64), nil},
		{"GET", base + "heat", nil},
		{"GET", base + "shares", nil},
		{"GET", base + "permissions", nil},
		{"GET", base + "store/list?prefix=journal/", nil},
		{"GET", base + "store/object?key=journal/d.jsonl", nil},
		{"GET", base + "store/exists?key=journal/d.jsonl", nil},
		{"POST", base + "reads", map[string]any{"reads": []any{}}},
	}
}

// secpermShare mints a live share on the project without going through the
// write API (the file need not be synced for the token to exist).
func secpermShare(t *testing.T, srv *Server, projectID string) Share {
	t.Helper()
	sh, err := srv.Shares.Create(projectID, "x.md", "alice@x.io", 0, FileInfo{})
	if err != nil {
		t.Fatal(err)
	}
	return sh
}

// secpermNewProject creates a project as who and returns it.
func secpermNewProject(t *testing.T, h http.Handler, c *http.Cookie, org, name string) Project {
	t.Helper()
	rec := doAs(t, h, "POST", "/api/projects", map[string]any{"name": name, "org": org}, c)
	if rec.Code != 200 {
		t.Fatalf("create project %q: %d %s", name, rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Project
}

// ---- row 2: read cannot write ----

// A read-only member must be refused every PermWrite action, and the same
// request must succeed for an admin — the delta is what proves the shape is
// valid and the 403 is the server's decision.
func TestSec_Perms_ReadOnlyMemberCannotWrite(t *testing.T) {
	h, srv, c, p := permHub(t)
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	for _, rt := range secpermWrites(p.ID, secpermShare(t, srv, p.ID).Token) {
		if rec := doAs(t, h, rt.method, rt.url, rt.body, c["alice"]); rec.Code == http.StatusForbidden {
			t.Fatalf("control: %s %s as the org owner: 403 %s", rt.method, rt.url, rec.Body)
		}
		if rec := doAs(t, h, rt.method, rt.url, rt.body, c["bob"]); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as a read-only member: %d, want 403 (%s)", rt.method, rt.url, rec.Code, rec.Body)
		}
	}
}

// ---- row 2: write cannot administer ----

// A plain write member must be refused every PermAdmin action.
func TestSec_Perms_WriteMemberCannotAdmin(t *testing.T) {
	h, _, c, p := permHub(t)
	// carol is a plain org member: the project default (write) applies.
	victim := secpermNewProject(t, h, c["alice"], p.Org, "victim")
	control := secpermNewProject(t, h, c["alice"], p.Org, "control")
	for _, rt := range secpermAdmins(control.ID) {
		if rec := doAs(t, h, rt.method, rt.url, rt.body, c["alice"]); rec.Code == http.StatusForbidden {
			t.Fatalf("control: %s %s as the org owner: 403 %s", rt.method, rt.url, rec.Body)
		}
	}
	for _, rt := range secpermAdmins(victim.ID) {
		url := strings.ReplaceAll(rt.url, control.ID, victim.ID)
		if rec := doAs(t, h, rt.method, url, rt.body, c["carol"]); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as a write member: %d, want 403 (%s)", rt.method, url, rec.Code, rec.Body)
		}
	}
}

// ---- row 2: none reaches nothing, including the org-wide share list ----

// A "none" grant must hide the project everywhere — including from the
// org-wide share audit, whose rows carry the public /s/ token for every file
// somebody shared. handleOrgShares gates on org membership only, so a member
// cut off from a project still receives its live share tokens (and can then
// read the file contents through the unauthenticated /s/ route).
func TestSec_Perms_NoneMemberCannotListProjectSharesViaOrg(t *testing.T) {
	h, srv, c, p := permHub(t)
	sh := secpermShare(t, srv, p.ID)
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermNone); err != nil {
		t.Fatal(err)
	}
	// control: alice, who administers the project, does see it.
	rec := doAs(t, h, "GET", "/api/orgs/"+p.Org+"/shares", nil, c["alice"])
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), sh.Token) {
		t.Fatalf("control: org owner share audit: %d %s", rec.Code, rec.Body)
	}
	rec = doAs(t, h, "GET", "/api/orgs/"+p.Org+"/shares", nil, c["bob"])
	if strings.Contains(rec.Body.String(), sh.Token) {
		t.Errorf("a none-level member reads the share token for a project they are walled out of: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), p.ID) {
		t.Errorf("a none-level member learns the project exists from the org share audit: %s", rec.Body)
	}
	// and an outsider is refused outright
	if rec := doAs(t, h, "GET", "/api/orgs/"+p.Org+"/shares", nil, c["dave"]); rec.Code != http.StatusForbidden {
		t.Errorf("outsider org share audit: %d, want 403", rec.Code)
	}
}

// Every project route must refuse a "none" member, read and write alike.
func TestSec_Perms_NoneMemberReachesNothing(t *testing.T) {
	h, srv, c, p := permHub(t)
	tok := secpermShare(t, srv, p.ID).Token
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermNone); err != nil {
		t.Fatal(err)
	}
	all := append(secpermReads(p.ID), secpermWrites(p.ID, tok)...)
	for _, rt := range all {
		if rec := doAs(t, h, rt.method, rt.url, rt.body, c["bob"]); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as a none member: %d, want 403 (%s)", rt.method, rt.url, rec.Code, rec.Body)
		}
	}
	if rec := doAs(t, h, "GET", "/api/projects/"+p.ID, nil, c["bob"]); rec.Code == 200 {
		t.Errorf("GET /api/projects/{id} as a none member: 200 %s", rec.Body)
	}
}

// ---- row 2: offboarding ----

// Removing an account from the organization must revoke its access to that
// org's projects. An explicit grant is checked before org membership in
// projectPerm, so a grant written while the account was a member outlives the
// membership: the offboarded account keeps read+write on the project.
func TestSec_Perms_RemovedOrgMemberLosesProjectAccess(t *testing.T) {
	h, srv, c, p := permHub(t)
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/bob@x.io",
		map[string]string{"level": "write"}, c["alice"]); rec.Code != 200 {
		t.Fatalf("grant bob write: %d %s", rec.Code, rec.Body)
	}
	// control: while he is a member the grant works.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["bob"]); rec.Code != 200 {
		t.Fatalf("control: granted member read: %d %s", rec.Code, rec.Body)
	}
	// alice offboards bob from the organization.
	if rec := doAs(t, h, "DELETE", "/api/orgs/"+p.Org+"/members/bob@x.io", nil, c["alice"]); rec.Code != 200 {
		t.Fatalf("remove member: %d %s", rec.Code, rec.Body)
	}
	if role := srv.Dir.Role(p.Org, "bob@x.io"); role != "" {
		t.Fatalf("bob is still in the org with role %q", role)
	}
	for _, rt := range append(secpermReads(p.ID), secpermReq{"PUT", "/api/p/" + p.ID + "/store/object?key=journal/d.jsonl", []byte("{}")}) {
		if rec := doAs(t, h, rt.method, rt.url, rt.body, c["bob"]); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s after removal from the org: %d, want 403 (%s)", rt.method, rt.url, rec.Code, rec.Body)
		}
	}
	if rec := doAs(t, h, "GET", "/api/projects", nil, c["bob"]); strings.Contains(rec.Body.String(), p.ID) {
		t.Errorf("removed member still sees the project in /api/projects: %s", rec.Body)
	}
}

// ---- row 2: the fail-open escapes ----

// projectPerm returns admin for a project whose Org is empty. No API path on
// an auth+orgs hub produces one (asserted below), but the resolver must fail
// closed rather than rely on that: an operator-written row, an import, or a
// directory backend that hands back a project before the startup migration
// would otherwise be world-writable by every account on the hub.
func TestSec_Perms_OrgLessProjectIsNotAdminForEveryone(t *testing.T) {
	h, srv, c, p := permHub(t)

	// (a) no API path leaves a project org-less on a configured hub. (Only
	// projects this test creates through the API count — permHub's underlying
	// newHub fixture seeds one org-less project directly in the registry.)
	made := []Project{
		secpermNewProject(t, h, c["dave"], "", "daves-own"),   // no org named
		secpermNewProject(t, h, c["bob"], p.Org, "bobs-own"),  // org named
		secpermNewProject(t, h, c["carol"], "", "carols-own"), // member of one org
	}
	for _, got := range made {
		if got.Org == "" {
			t.Errorf("project %q (%s) has no org after API creation", got.Name, got.ID)
		}
	}

	// (b) the resolver itself must still fail closed.
	if err := srv.Projects.SetOrg(p.ID, ""); err != nil {
		t.Fatal(err)
	}
	if lvl := srv.projectPerm(secpermAs(t, h, c["dave"]), p.ID); lvl == PermAdmin {
		t.Errorf("an org-less project resolves to admin for an unrelated account (level %q)", lvl)
	}
	if rec := doAs(t, h, "DELETE", "/api/projects/"+p.ID, nil, c["dave"]); rec.Code != http.StatusForbidden {
		t.Errorf("outsider DELETE of an org-less project: %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

// secpermAs builds a request carrying who's session, for calling resolver
// internals directly.
func secpermAs(t *testing.T, h http.Handler, c *http.Cookie) *http.Request {
	t.Helper()
	req, err := http.NewRequest("GET", "/api/projects", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(c)
	return req
}

// A grant holding an unrecognized level must resolve to none, not to whatever
// the string happens to be. Injected through the file backend, which is how a
// corrupt or hand-edited row actually arrives.
func TestSec_Perms_CorruptGrantFailsClosed(t *testing.T) {
	h, srv, c, p := permHub(t)
	path := filepath.Join(t.TempDir(), "projects.json")
	got, _ := srv.Projects.Get(p.ID)
	got.Perms = map[string]string{"bob@x.io": "ADMIN", "carol@x.io": "superuser"}
	blob, err := json.Marshal(map[string]any{"projects": []Project{got}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := OpenProjectDB(path)
	if err != nil {
		t.Fatal(err)
	}
	srv.Projects = db
	for _, who := range []string{"bob", "carol"} {
		for _, rt := range secpermAdmins(p.ID) {
			if rec := doAs(t, h, rt.method, rt.url, rt.body, c[who]); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s with a corrupt %s grant: %d, want 403 (%s)", rt.method, rt.url, who, rec.Code, rec.Body)
			}
		}
	}
}

// ---- row 4: cross-org isolation ----

// dave, a member of another org, must be refused every per-project route for
// alice's project — and every one of those requests must succeed for alice.
func TestSec_CrossOrg_ProjectRoutesRefuseOutsider(t *testing.T) {
	h, srv, c, p := permHub(t)
	tok := secpermShare(t, srv, p.ID).Token
	for _, rt := range append(secpermReads(p.ID), secpermWrites(p.ID, tok)...) {
		if rec := doAs(t, h, rt.method, rt.url, rt.body, c["alice"]); rec.Code == http.StatusForbidden {
			t.Fatalf("control: %s %s as the owner: 403 %s", rt.method, rt.url, rec.Body)
		}
		if rec := doAs(t, h, rt.method, rt.url, rt.body, c["dave"]); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s from another org: %d, want 403 (%s)", rt.method, rt.url, rec.Code, rec.Body)
		}
	}
	// project metadata: existence must not leak either
	if rec := doAs(t, h, "GET", "/api/projects/"+p.ID, nil, c["dave"]); rec.Code == 200 {
		t.Errorf("GET /api/projects/{id} from another org: 200 %s", rec.Body)
	}
	if rec := doAs(t, h, "GET", "/api/projects", nil, c["dave"]); strings.Contains(rec.Body.String(), p.ID) {
		t.Errorf("/api/projects leaks another org's project: %s", rec.Body)
	}
	// admin routes, which sit outside the proj() wrapper
	for _, rt := range secpermAdmins(p.ID) {
		if rec := doAs(t, h, rt.method, rt.url, rt.body, c["dave"]); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s from another org: %d, want 403 (%s)", rt.method, rt.url, rec.Code, rec.Body)
		}
	}
	// and create-or-join by name must not hand back the id
	if rec := doAs(t, h, "POST", "/api/projects",
		map[string]any{"name": p.Name, "org": p.Org}, c["dave"]); rec.Code == 200 {
		t.Errorf("join-by-name into another org: 200 %s", rec.Body)
	}
}

// The org routes must refuse both an outsider and a non-owner member, and
// /api/orgs must not disclose an org the caller is not in.
func TestSec_CrossOrg_OrgRoutesRefuseOutsiderAndNonOwner(t *testing.T) {
	h, srv, c, p := permHub(t)
	org, _ := srv.Dir.Get(p.Org)
	writes := []secpermReq{
		{"PATCH", "/api/orgs/" + p.Org, map[string]string{"name": "pwned"}},
		{"POST", "/api/orgs/" + p.Org + "/invites", map[string]string{}},
		{"GET", "/api/orgs/" + p.Org + "/invites", nil},
		{"PATCH", "/api/orgs/" + p.Org + "/members/carol@x.io", map[string]string{"role": "owner"}},
		{"DELETE", "/api/orgs/" + p.Org + "/members/carol@x.io", nil},
	}
	for _, rt := range writes {
		if rec := doAs(t, h, rt.method, rt.url, rt.body, c["alice"]); rec.Code == http.StatusForbidden {
			t.Fatalf("control: %s %s as the org owner: 403 %s", rt.method, rt.url, rec.Body)
		}
	}
	for _, who := range []string{"bob", "dave"} { // non-owner member, and an outsider
		for _, rt := range writes {
			if rec := doAs(t, h, rt.method, rt.url, rt.body, c[who]); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s as %s: %d, want 403 (%s)", rt.method, rt.url, who, rec.Code, rec.Body)
			}
		}
	}
	// existence, name and member emails of an org dave is not in
	rec := doAs(t, h, "GET", "/api/orgs", nil, c["dave"])
	for _, leak := range []string{p.Org, org.Name, "alice@x.io", "bob@x.io", "carol@x.io"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("/api/orgs leaks %q to an outsider: %s", leak, rec.Body)
		}
	}
}
