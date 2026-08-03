package webapp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Offensive tests for scoreboard rows 1 (the auth gate) and 3 (routes
// registered outside the proj() wrapper). Every helper here is prefixed
// "gate" so this file cannot collide with another attacker's file in the
// same package. Nothing in here is fixed — these assert the SECURE behavior
// and go green when the hole is closed.

// gateBody is the response body as a string.
func gateBody(rec interface{ String() string }) string { return rec.String() }

// gateShare mints a live share on a project straight through the DB — the
// same shortcut perms_test.go uses, since HTTP share creation needs a synced
// file and the share is only the bait here.
func gateShare(t *testing.T, srv *Server, project, path string) Share {
	t.Helper()
	sh, err := srv.Shares.Create(project, path, "alice@x.io", 0)
	if err != nil {
		t.Fatal(err)
	}
	return sh
}

// ---------------------------------------------------------------------------
// Row 1 — the auth gate's open-path rule
// ---------------------------------------------------------------------------

// The gate opens anything that does not literally start with "/api/". An
// anonymous caller must not be able to spell a gated route in a way that
// routes to its handler while reading as "open" — nor get project data back
// from the SPA fallback that catches the leftovers.
func TestSec_AuthGate_AnonymousPathTricksCannotReadAPI(t *testing.T) {
	h, _, c, p := permHub(t)

	// Control: the same URL with alice's cookie really does return the data,
	// so a refusal below is the gate's decision and not a broken request.
	if rec := doAs(t, h, "GET", "/api/projects", nil, c["alice"]); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), p.ID) {
		t.Fatalf("control: alice GET /api/projects = %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "GET", "/api/projects", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("control: anonymous GET /api/projects = %d, want 401", rec.Code)
	}

	tricks := []string{
		"/api/projects",
		"//api/projects",
		"///api/projects",
		"/api/../api/projects",
		"/x/../api/projects",
		"/./api/projects",
		"/api/./projects",
		"/api/projects/../projects",
		"/%2fapi/projects",
		"/api%2fprojects",
		"/%61pi/projects",          // %61 == 'a'
		"/x/..;/api/projects",      // ..; is not a Go path element
		"/api/projects.",           // trailing dot
		"/api/projects%20",         // trailing space
		"/api/projects%00",         // NUL
		"/API/projects",            // case
		"/api/p/" + p.ID + "/tree", // per-project read
		"/x/../api/p/" + p.ID + "/tree",
		"//api/p/" + p.ID + "/tree",
		"/api/p/" + p.ID + "/permissions",
		"/x/../api/p/" + p.ID + "/permissions",
		"/api/orgs",
		"/x/../api/orgs",
		"/api/projects/" + p.ID,
		"/x/../api/projects/" + p.ID,
	}
	for _, u := range tricks {
		rec := doAs(t, h, "GET", u, nil, nil)
		// A redirect serves no handler; its body only echoes the Location. What
		// matters is that the place it sends you is itself gated.
		if rec.Code >= 300 && rec.Code < 400 {
			if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/api/") {
				t.Errorf("anonymous GET %q redirects to an ungated %q", u, loc)
			}
			continue
		}
		body := gateBody(rec.Body)
		if strings.Contains(body, p.ID) || strings.Contains(body, "alice@x.io") {
			t.Errorf("anonymous GET %q leaked project data: %d %s", u, rec.Code, body)
		}
	}
}

// Same rule, but for writes: a path form that the gate reads as open must not
// be able to destroy a project.
func TestSec_AuthGate_AnonymousPathTricksCannotWrite(t *testing.T) {
	h, srv, c, p := permHub(t)

	for _, u := range []string{
		"/api/projects/" + p.ID,
		"//api/projects/" + p.ID,
		"/x/../api/projects/" + p.ID,
		"/api/../api/projects/" + p.ID,
		"/%2fapi/projects/" + p.ID,
		"/x/..;/api/projects/" + p.ID,
		"/api/./projects/" + p.ID,
	} {
		for _, m := range []string{"DELETE", "PATCH"} {
			doAs(t, h, m, u, map[string]string{"name": "pwned"}, nil)
		}
		got, ok := srv.Projects.Get(p.ID)
		if !ok {
			t.Fatalf("anonymous %q deleted the project", u)
		}
		if got.Name != p.Name {
			t.Fatalf("anonymous %q renamed the project to %q", u, got.Name)
		}
	}
	// Control: alice really can do it, so the shape above was valid.
	if rec := doAs(t, h, "PATCH", "/api/projects/"+p.ID, map[string]string{"name": "renamed"}, c["alice"]); rec.Code != 200 {
		t.Fatalf("control: alice PATCH project = %d %s", rec.Code, rec.Body)
	}
}

// /api/config is the one gated-prefix path deliberately left open. It must
// stay free of anything an anonymous stranger should not learn: accounts,
// storage, admin state.
func TestSec_AuthGate_ConfigLeaksNothingToAnonymous(t *testing.T) {
	h, srv, _, p := permHub(t)
	srv.Auth.(*BuiltinAuth).Admins = map[string]bool{"alice@x.io": true}

	rec := doAs(t, h, "GET", "/api/config", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("config: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, secret := range []string{
		"alice@x.io", "bob@x.io", "dave@x.io", // accounts
		p.ID, p.Org, // project/org inventory (not p.Name: "wiki" is also a template name)
		"file://", "/var/folders", "/tmp/", // storage location
		`"admin":true`, // admin state of an anonymous caller
		`"me"`,         // identity block
	} {
		if strings.Contains(body, secret) {
			t.Errorf("anonymous /api/config leaks %q: %s", secret, body)
		}
	}
}

// ---------------------------------------------------------------------------
// Row 3 — routes registered OUTSIDE the proj() wrapper
// ---------------------------------------------------------------------------

// PATCH/DELETE /api/shares/{token}: re-dating and killing a public link is the
// same authority as minting one, so an outsider must be refused both.
func TestSec_Row3_ShareMutationByOutsider(t *testing.T) {
	h, srv, c, p := permHub(t)
	sh := gateShare(t, srv, p.ID, "x.md")

	for _, tc := range []struct {
		method string
		body   any
	}{
		{"PATCH", map[string]string{"expires_in": "24h"}},
		{"DELETE", nil},
	} {
		if rec := doAs(t, h, tc.method, "/api/shares/"+sh.Token, tc.body, c["dave"]); rec.Code != http.StatusForbidden {
			t.Errorf("dave (other org) %s /api/shares/{token}: %d, want 403 (%s)", tc.method, rec.Code, rec.Body)
		}
	}
	if _, ok := srv.Shares.Get(sh.Token); !ok {
		t.Fatal("dave revoked a share in another org's project")
	}
	// A plain org member demoted to read on the project must be refused too.
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	if rec := doAs(t, h, "DELETE", "/api/shares/"+sh.Token, nil, c["bob"]); rec.Code != http.StatusForbidden {
		t.Errorf("read-only bob DELETE /api/shares/{token}: %d, want 403", rec.Code)
	}
	// Control: alice may.
	if rec := doAs(t, h, "DELETE", "/api/shares/"+sh.Token, nil, c["alice"]); rec.Code != 200 {
		t.Fatalf("control: alice DELETE share = %d %s", rec.Code, rec.Body)
	}
}

// handleShareRevoke only runs the permission check when ShareDB.Get resolves
// the token — and Get refuses expired shares. An expired share row still
// exists, and ShareDB.Revoke deletes it regardless, so the check is skipped
// exactly when the row is still there to delete.
func TestSec_Row3_ExpiredShareRevokableByOutsider(t *testing.T) {
	h, srv, c, p := permHub(t)
	sh, err := srv.Shares.Create(p.ID, "x.md", "alice@x.io", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Age it out in place: the row survives, Get stops resolving it.
	srv.Shares.mu.Lock()
	aged := srv.Shares.byToken[sh.Token]
	aged.Expires = time.Now().UTC().Add(-time.Hour)
	srv.Shares.byToken[sh.Token] = aged
	srv.Shares.mu.Unlock()

	rec := doAs(t, h, "DELETE", "/api/shares/"+sh.Token, nil, c["dave"])
	if rec.Code == 200 {
		t.Errorf("dave (other org) revoked an expired share: %d %s", rec.Code, rec.Body)
	}
	srv.Shares.mu.Lock()
	_, still := srv.Shares.byToken[sh.Token]
	srv.Shares.mu.Unlock()
	if !still {
		t.Errorf("dave (other org) deleted the share row for project %s with no permission check", p.ID)
	}
}

// All four /api/p/{project}/permissions routes sit outside proj() and check
// themselves. An outsider gets nothing; a plain write member cannot edit them.
func TestSec_Row3_PermissionRoutes(t *testing.T) {
	h, _, c, p := permHub(t)
	base := "/api/p/" + p.ID + "/permissions"
	routes := []struct {
		method, url string
		body        any
	}{
		{"GET", base, nil},
		{"PUT", base, map[string]string{"default": "read"}},
		{"PUT", base + "/carol@x.io", map[string]string{"level": "read"}},
		{"DELETE", base + "/carol@x.io", nil},
	}
	for _, rt := range routes {
		if rec := doAs(t, h, rt.method, rt.url, rt.body, c["dave"]); rec.Code != http.StatusForbidden {
			t.Errorf("dave (other org) %s %s: %d, want 403 (%s)", rt.method, rt.url, rec.Code, rec.Body)
		}
	}
	// bob is a plain member at the default level (write). Editing permissions
	// is admin work; only GET is his.
	for _, rt := range routes[1:] {
		if rec := doAs(t, h, rt.method, rt.url, rt.body, c["bob"]); rec.Code != http.StatusForbidden {
			t.Errorf("write-level bob %s %s: %d, want 403 (%s)", rt.method, rt.url, rec.Code, rec.Body)
		}
	}
	// Control: alice can, so the bodies above are well-formed.
	if rec := doAs(t, h, "PUT", base+"/carol@x.io", map[string]string{"level": "read"}, c["alice"]); rec.Code != 200 {
		t.Fatalf("control: alice grant = %d %s", rec.Code, rec.Body)
	}
}

// PATCH/DELETE /api/projects/{project} and GET /api/projects/{project}.
func TestSec_Row3_ProjectLifecycleRoutes(t *testing.T) {
	h, srv, c, p := permHub(t)

	// dave: not in the org at all.
	if rec := doAs(t, h, "GET", "/api/projects/"+p.ID, nil, c["dave"]); rec.Code == 200 ||
		strings.Contains(rec.Body.String(), p.Name) {
		t.Errorf("dave GET /api/projects/{id}: %d %s, want a refusal with no project data", rec.Code, rec.Body)
	}
	for _, tc := range []struct {
		method string
		body   any
	}{
		{"PATCH", map[string]string{"name": "pwned"}},
		{"DELETE", nil},
	} {
		if rec := doAs(t, h, tc.method, "/api/projects/"+p.ID, tc.body, c["dave"]); rec.Code != http.StatusForbidden {
			t.Errorf("dave %s /api/projects/{id}: %d, want 403 (%s)", tc.method, rec.Code, rec.Body)
		}
		// bob is a plain write member; rename and delete are admin work.
		if rec := doAs(t, h, tc.method, "/api/projects/"+p.ID, tc.body, c["bob"]); rec.Code != http.StatusForbidden {
			t.Errorf("write-level bob %s /api/projects/{id}: %d, want 403 (%s)", tc.method, rec.Code, rec.Body)
		}
	}
	if got, ok := srv.Projects.Get(p.ID); !ok || got.Name != p.Name {
		t.Fatalf("project mutated by a non-admin: %+v ok=%v", got, ok)
	}
	// dave must not be able to reach the org either by naming it on create.
	if rec := doAs(t, h, "POST", "/api/projects", map[string]any{"name": "trojan", "org": p.Org}, c["dave"]); rec.Code != http.StatusForbidden {
		t.Errorf("dave created a project in alice's org: %d %s", rec.Code, rec.Body)
	}
	// Control.
	if rec := doAs(t, h, "PATCH", "/api/projects/"+p.ID, map[string]string{"name": "ok"}, c["alice"]); rec.Code != 200 {
		t.Fatalf("control: alice PATCH = %d %s", rec.Code, rec.Body)
	}
}

// The /api/orgs/* family: every write is owner-only, and an outsider sees
// nothing at all.
func TestSec_Row3_OrgRoutes(t *testing.T) {
	h, _, c, p := permHub(t)

	// alice mints an invite (control that the shape works).
	rec := doAs(t, h, "POST", "/api/orgs/"+p.Org+"/invites", map[string]string{"expires_in": "168h"}, c["alice"])
	if rec.Code != 200 {
		t.Fatalf("control: alice invite create = %d %s", rec.Code, rec.Body)
	}
	var inv struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &inv)

	ownerOnly := []struct {
		method, url string
		body        any
	}{
		{"PATCH", "/api/orgs/" + p.Org, map[string]string{"name": "pwned"}},
		{"POST", "/api/orgs/" + p.Org + "/invites", nil},
		{"GET", "/api/orgs/" + p.Org + "/invites", nil},
		{"DELETE", "/api/orgs/" + p.Org + "/invites/" + inv.Token, nil},
		{"PATCH", "/api/orgs/" + p.Org + "/members/carol@x.io", map[string]string{"role": "owner"}},
		{"DELETE", "/api/orgs/" + p.Org + "/members/carol@x.io", nil},
	}
	for _, who := range []string{"dave", "bob"} {
		for _, rt := range ownerOnly {
			if rec := doAs(t, h, rt.method, rt.url, rt.body, c[who]); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s %s: %d, want 403 (%s)", who, rt.method, rt.url, rec.Code, rec.Body)
			}
		}
	}
	// The org-wide share audit is member-visible; an outsider is walled out.
	if rec := doAs(t, h, "GET", "/api/orgs/"+p.Org+"/shares", nil, c["dave"]); rec.Code != http.StatusForbidden {
		t.Errorf("dave GET /api/orgs/{org}/shares: %d, want 403 (%s)", rec.Code, rec.Body)
	}
	// /api/orgs must not enumerate orgs the caller is not in.
	if rec := doAs(t, h, "GET", "/api/orgs", nil, c["dave"]); strings.Contains(rec.Body.String(), p.Org) {
		t.Errorf("dave sees alice's org in /api/orgs: %s", rec.Body)
	}
}

// The org-wide share audit (GET /api/orgs/{org}/shares) checks org membership
// only — it never consults the per-project level. A member cut off from a
// project (level "none", which the model defines as "hidden: absent from the
// list, 403 everywhere") must not learn its name, its file paths, or, worst,
// a working public /s/ link to its contents.
func TestSec_Row3_OrgSharesLeaksDeniedProject(t *testing.T) {
	h, srv, c, p := permHub(t)
	sh := gateShare(t, srv, p.ID, "secret.md")
	if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermNone); err != nil {
		t.Fatal(err)
	}
	// Baseline: the per-project route obeys the level.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/shares", nil, c["bob"]); rec.Code != http.StatusForbidden {
		t.Fatalf("baseline: none-level bob GET project shares = %d, want 403", rec.Code)
	}
	// Control: alice, who may see it, does.
	if rec := doAs(t, h, "GET", "/api/orgs/"+p.Org+"/shares", nil, c["alice"]); !strings.Contains(rec.Body.String(), sh.Token) {
		t.Fatalf("control: alice org shares = %d %s", rec.Code, rec.Body)
	}
	rec := doAs(t, h, "GET", "/api/orgs/"+p.Org+"/shares", nil, c["bob"])
	for _, leak := range []string{sh.Token, "secret.md", p.ID, p.Name} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("org share audit leaks %q from a project bob has no access to: %s", leak, rec.Body)
		}
	}
}

// POST /api/invites/{token}: the token is the whole authorization, so a
// revoked one must stop working and a forged one must never join anybody.
func TestSec_Row3_InviteAccept(t *testing.T) {
	h, srv, c, p := permHub(t)

	if rec := doAs(t, h, "POST", "/api/invites/deadbeefdeadbeef", nil, c["dave"]); rec.Code != http.StatusNotFound {
		t.Errorf("forged invite token: %d, want 404 (%s)", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "POST", "/api/invites/deadbeefdeadbeef", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous invite accept: %d, want 401 (%s)", rec.Code, rec.Body)
	}

	rec := doAs(t, h, "POST", "/api/orgs/"+p.Org+"/invites", nil, c["alice"])
	if rec.Code != 200 {
		t.Fatalf("mint invite: %d %s", rec.Code, rec.Body)
	}
	var inv struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &inv)
	if rec := doAs(t, h, "DELETE", "/api/orgs/"+p.Org+"/invites/"+inv.Token, nil, c["alice"]); rec.Code != 200 {
		t.Fatalf("revoke invite: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "POST", "/api/invites/"+inv.Token, nil, c["dave"]); rec.Code == 200 {
		t.Errorf("revoked invite still joins: %d %s", rec.Code, rec.Body)
	}
	if srv.Dir.Role(p.Org, "dave@x.io") != "" {
		t.Errorf("dave joined alice's org through a revoked invite")
	}
	// Control: a live invite does work, so the refusal above is the revocation.
	rec = doAs(t, h, "POST", "/api/orgs/"+p.Org+"/invites", nil, c["alice"])
	json.Unmarshal(rec.Body.Bytes(), &inv)
	if rec := doAs(t, h, "POST", "/api/invites/"+inv.Token, nil, c["dave"]); rec.Code != 200 {
		t.Fatalf("control: live invite = %d %s", rec.Code, rec.Body)
	}
}

// /api/admin/* is hub-admin only; org ownership buys nothing here.
func TestSec_Row3_AdminRoutes(t *testing.T) {
	h, srv, c, _ := permHub(t)
	routes := []struct {
		method, url string
		body        any
	}{
		{"GET", "/api/admin/policy", nil},
		{"POST", "/api/admin/policy", map[string]bool{"require_approval": false}},
		{"GET", "/api/admin/pending", nil},
		{"POST", "/api/admin/pending/nobody/approve", nil},
		{"POST", "/api/admin/pending/nobody/deny", nil},
	}
	for _, who := range []string{"dave", "bob", "alice"} {
		for _, rt := range routes {
			if rec := doAs(t, h, rt.method, rt.url, rt.body, c[who]); rec.Code != http.StatusForbidden {
				t.Errorf("non-admin %s %s %s: %d, want 403 (%s)", who, rt.method, rt.url, rec.Code, rec.Body)
			}
		}
		if rec := doAs(t, h, "GET", "/api/admin/pending", nil, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous admin pending: %d, want 401", rec.Code)
		}
	}
	// Control: promote alice to hub admin and the same call succeeds, so the
	// 403s above are the admin check and not a broken route.
	srv.Auth.(*BuiltinAuth).Admins = map[string]bool{"alice@x.io": true}
	if rec := doAs(t, h, "GET", "/api/admin/pending", nil, c["alice"]); rec.Code != 200 {
		t.Fatalf("control: hub admin GET /api/admin/pending = %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "GET", "/api/admin/pending", nil, c["bob"]); rec.Code != http.StatusForbidden {
		t.Errorf("bob still not an admin: %d", rec.Code)
	}
}
