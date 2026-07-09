package webapp

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
)

func TestOrgLifecycle(t *testing.T) {
	db, _ := OpenOrgDB(filepath.Join(t.TempDir(), "orgs.json"))
	o, _ := db.Create("acme", "alice@x.io")
	db.AddMember(o.ID, "bob@x.io", RoleMember)

	// rename
	if err := db.Rename(o.ID, "Acme Inc"); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.Get(o.ID); got.Name != "Acme Inc" {
		t.Fatalf("rename failed: %q", got.Name)
	}
	// promote bob, then the last-owner guard
	if err := db.SetRole(o.ID, "bob@x.io", RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRole(o.ID, "alice@x.io", RoleMember); err != nil {
		t.Fatal(err) // ok: bob is still an owner
	}
	if err := db.SetRole(o.ID, "bob@x.io", RoleMember); err == nil {
		t.Fatal("demoting the last owner must be refused")
	}
	// remove member (bob is the only owner now)
	if err := db.RemoveMember(o.ID, "alice@x.io"); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveMember(o.ID, "bob@x.io"); err == nil {
		t.Fatal("removing the last owner must be refused")
	}
	// invite revoke
	inv, _ := db.CreateInvite(o.ID, "bob@x.io", 0)
	if got := db.ListInvites(o.ID); len(got) != 1 {
		t.Fatalf("invite list = %d", len(got))
	}
	if !db.RevokeInvite(inv.Token) {
		t.Fatal("revoke returned false")
	}
	if _, ok := db.Redeem(inv.Token); ok {
		t.Fatal("revoked invite still redeems")
	}
}

func TestProjectLifecycle(t *testing.T) {
	db, _ := OpenProjectDB(filepath.Join(t.TempDir(), "projects.json"))
	p, _, _ := db.GetOrCreate("wiki", "o-1")
	db.GetOrCreate("docs", "o-1")

	if err := db.Rename(p.ID, "handbook"); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.Get(p.ID); got.Name != "handbook" {
		t.Fatalf("rename: %q", got.Name)
	}
	// name collision within the org is refused
	if err := db.Rename(p.ID, "docs"); err == nil {
		t.Fatal("rename to an existing org-name must be refused")
	}
	if err := db.Delete(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.Get(p.ID); ok {
		t.Fatal("deleted project still present")
	}
}

// Owner-only guards on the HTTP surface: a plain member is refused, an owner
// succeeds.
func TestAdminEndpointsOwnerOnly(t *testing.T) {
	h, _, alice, bob, pa := orgHubSrv(t)

	// bob is not even a member of alice's org → 403 on rename
	if rec := doAs(t, h, "PATCH", "/api/orgs/"+pa.Org, map[string]string{"name": "x"}, bob); rec.Code != http.StatusForbidden {
		t.Fatalf("non-member org rename: %d", rec.Code)
	}
	// alice (owner) can rename her org
	if rec := doAs(t, h, "PATCH", "/api/orgs/"+pa.Org, map[string]string{"name": "Alice Co"}, alice); rec.Code != 200 {
		t.Fatalf("owner org rename: %d %s", rec.Code, rec.Body)
	}
	// alice can rename her project
	if rec := doAs(t, h, "PATCH", "/api/projects/"+pa.ID, map[string]string{"name": "notes"}, alice); rec.Code != 200 {
		t.Fatalf("owner project rename: %d %s", rec.Code, rec.Body)
	}
	// bob cannot delete alice's project (not a member → 404, doesn't leak)
	if rec := doAs(t, h, "DELETE", "/api/projects/"+pa.ID, nil, bob); rec.Code == 200 {
		t.Fatal("non-member deleted a project")
	}
	// alice can delete it
	if rec := doAs(t, h, "DELETE", "/api/projects/"+pa.ID, nil, alice); rec.Code != 200 {
		t.Fatalf("owner project delete: %d %s", rec.Code, rec.Body)
	}
}

// The invite→join→role→remove flow over HTTP, end to end.
func TestMemberManagementHTTP(t *testing.T) {
	h, _, alice, bob, pa := orgHubSrv(t)

	// invite bob and have him join
	rec := doAs(t, h, "POST", "/api/orgs/"+pa.Org+"/invites", nil, alice)
	var inv struct {
		Token string `json:"token"`
	}
	mustJSON(t, rec, &inv)
	if rec := doAs(t, h, "POST", "/api/invites/"+inv.Token, nil, bob); rec.Code != 200 {
		t.Fatalf("bob join: %d %s", rec.Code, rec.Body)
	}
	// alice promotes bob to owner
	if rec := doAs(t, h, "PATCH", "/api/orgs/"+pa.Org+"/members/bob@x.io", map[string]string{"role": "owner"}, alice); rec.Code != 200 {
		t.Fatalf("promote: %d %s", rec.Code, rec.Body)
	}
	// alice removes bob
	if rec := doAs(t, h, "DELETE", "/api/orgs/"+pa.Org+"/members/bob@x.io", nil, alice); rec.Code != 200 {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	// bob is out: his project list no longer shows it
	rec = doAs(t, h, "GET", "/api/projects", nil, bob)
	var list struct {
		Projects []Project `json:"projects"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list)
	for _, p := range list.Projects {
		if p.ID == pa.ID {
			t.Fatal("removed member still sees the project")
		}
	}
}
