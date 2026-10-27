package webapp

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writerHub is the data hub with apps on: alice owns the org, bob and carol
// are members who can write the project, dave is in another org, and secret/
// is closed to everyone but alice.
func writerHub(t *testing.T) *dataHub {
	t.Helper()
	x, err := OpenDataIndex("sqlite", filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { x.Close() })
	d := newDataHub(t, x)
	apps, err := OpenAppDB(filepath.Join(t.TempDir(), "apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	d.srv.Apps = apps
	d.enable()
	return d
}

func keyAs(d *dataHub, app, who string) string {
	d.t.Helper()
	rec := d.send("POST", "/api/orgs/"+d.p.Org+"/apps/"+app+"/keys", "", who, nil, 200)
	var out struct{ Key string }
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Key
}

func (d *dataHub) grant(who, principal, body string, want int) {
	d.t.Helper()
	d.send("PUT", "/api/p/"+d.p.ID+"/permissions/"+principal, body, who, nil, want)
}

func (d *dataHub) dataQuery() string { return "/api/p/" + d.p.ID + "/data/query" }

func under(paths []string, prefix string) bool {
	return len(paths) > 0 && !slices.ContainsFunc(paths, func(p string) bool { return !strings.HasPrefix(p, prefix) })
}

// Any member creates apps; the member who created one, or an owner, manages it.
func TestMembersCreateAppsAndManageTheirOwn(t *testing.T) {
	d := writerHub(t)
	org := d.p.Org
	rec := d.send("POST", "/api/orgs/"+org+"/apps", `{"name":"Bob's server"}`, "bob", nil, 200)
	var made struct{ ID string }
	json.Unmarshal(rec.Body.Bytes(), &made)
	d.send("POST", "/api/orgs/"+org+"/apps", `{"name":"x"}`, "dave", nil, http.StatusForbidden)
	d.send("GET", "/api/orgs/"+org+"/apps", "", "dave", nil, http.StatusForbidden)

	keys := "/api/orgs/" + org + "/apps/" + made.ID + "/keys"
	d.send("POST", keys, "", "carol", nil, http.StatusForbidden)
	d.send("DELETE", "/api/orgs/"+org+"/apps/"+made.ID, "", "carol", nil, http.StatusForbidden)
	keyAs(d, made.ID, "bob")
	keyAs(d, made.ID, "alice") // an owner manages every app

	type listed struct {
		Apps []struct {
			ID     string
			Manage bool
			Keys   []struct{ ID string }
		}
	}
	list := func(who string) (manage bool, keyIDs []string) {
		t.Helper()
		var out listed
		json.Unmarshal(d.send("GET", "/api/orgs/"+org+"/apps", "", who, nil, 200).Body.Bytes(), &out)
		for _, a := range out.Apps {
			if a.ID == made.ID {
				for _, k := range a.Keys {
					keyIDs = append(keyIDs, k.ID)
				}
				return a.Manage, keyIDs
			}
		}
		t.Fatalf("%s does not see the app", who)
		return
	}
	if m, ks := list("carol"); m || len(ks) != 0 {
		t.Fatalf("carol sees manage=%v and %d keys of bob's app", m, len(ks))
	}
	m, ks := list("bob")
	if !m || len(ks) != 2 {
		t.Fatalf("bob sees manage=%v and %d keys of his app, want true and 2", m, len(ks))
	}
	d.send("DELETE", keys+"/"+ks[0], "", "carol", nil, http.StatusForbidden)
	d.send("DELETE", keys+"/"+ks[0], "", "bob", nil, 200)
	d.send("DELETE", "/api/orgs/"+org+"/apps/"+made.ID, "", "bob", nil, 200)
}

// A member who can write may give an app read, on the project or on a folder
// they can write, and nothing else; an admin's grant is not theirs to change.
func TestWritersGiveAppsReadAndNothingMore(t *testing.T) {
	d := writerHub(t)
	app, principal := newApp(d, d.p.Org, "bob", "B")
	key := keyAs(d, app, "alice")
	d.grant("bob", principal, `{"level":"write"}`, http.StatusForbidden)
	d.grant("bob", principal, `{"level":"admin"}`, http.StatusForbidden)
	d.grant("bob", "carol@x.io", `{"level":"read"}`, http.StatusForbidden)
	d.grant("bob", principal, `{"level":"read","path":"secret"}`, http.StatusForbidden) // bob cannot write secret/
	d.grant("bob", principal, `{"level":"read"}`, 200)
	paths := keyPaths(t, d, d.dataQuery(), key)
	if !slices.Contains(paths, "acme/placements/editorial/d.com.json") || slices.ContainsFunc(paths, func(p string) bool {
		return strings.HasPrefix(p, "secret/")
	}) {
		t.Fatalf("a writer's read grant reaches %v", paths)
	}

	// A member who can only read gives nothing.
	if err := d.srv.Projects.SetPerm(d.p.ID, "carol@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	other, otherPrincipal := newApp(d, d.p.Org, "carol", "C")
	_ = other
	d.grant("carol", otherPrincipal, `{"level":"read"}`, http.StatusForbidden)

	// An admin's grant is not a writer's to replace or remove.
	_, adminPrincipal := newApp(d, d.p.Org, "alice", "A")
	d.grant("alice", adminPrincipal, `{"level":"write"}`, 200)
	d.grant("bob", adminPrincipal, `{"level":"read"}`, http.StatusForbidden)
	d.send("DELETE", "/api/p/"+d.p.ID+"/permissions/"+adminPrincipal, "", "bob", nil, http.StatusForbidden)

	// Bob's own grant is his to move to a folder and to take back.
	d.send("DELETE", "/api/p/"+d.p.ID+"/permissions/"+principal, "", "bob", nil, 200)
	d.grant("bob", principal, `{"level":"read","path":"acme"}`, 200)
	if paths := keyPaths(t, d, d.dataQuery(), key); !under(paths, "acme/") {
		t.Fatalf("an app granted read on acme/ alone sees %v", paths)
	}
	// History is held to the same folder.
	rec := keyCall(d, "GET", "/api/p/"+d.p.ID+"/history", "", key)
	var hist struct{ Entries []struct{ Path string } }
	json.Unmarshal(rec.Body.Bytes(), &hist)
	var seen []string
	for _, e := range hist.Entries {
		seen = append(seen, e.Path)
	}
	if rec.Code != 200 || !under(seen, "acme/") {
		t.Fatalf("history for an app granted acme/ alone: %d %v", rec.Code, seen)
	}
	d.send("DELETE", "/api/p/"+d.p.ID+"/permissions/"+principal+"?path=acme", "", "bob", nil, 200)
	if rec := keyCall(d, "POST", d.dataQuery(), `{"pattern":"**/*.json"}`, key); rec.Code != http.StatusForbidden {
		t.Fatalf("an app with its grants taken back: %d %s", rec.Code, rec.Body)
	}
}

// An app's grant counts only while its giver could still give it, judged on
// every request: no cleanup when the giver leaves, is demoted, or is shut out
// of a folder.
func TestAppGrantsHoldOnlyWhileTheirGiverCould(t *testing.T) {
	setup := func(t *testing.T) (d *dataHub, key string) {
		d = writerHub(t)
		app, principal := newApp(d, d.p.Org, "bob", "B")
		key = keyAs(d, app, "alice") // an owner's key, so only the grant is on trial
		d.grant("bob", principal, `{"level":"read"}`, 200)
		if paths := keyPaths(t, d, d.dataQuery(), key); len(paths) == 0 {
			t.Fatal("the grant opened nothing")
		}
		return d, key
	}
	refused := func(t *testing.T, d *dataHub, key, why string) {
		t.Helper()
		if rec := keyCall(d, "POST", d.dataQuery(), `{"pattern":"**/*.json"}`, key); rec.Code != http.StatusForbidden {
			t.Fatalf("%s: the app still reads: %d %s", why, rec.Code, rec.Body)
		}
	}
	t.Run("giver removed from the org", func(t *testing.T) {
		d, key := setup(t)
		d.send("DELETE", "/api/orgs/"+d.p.Org+"/members/bob@x.io", "", "alice", nil, 200)
		refused(t, d, key, "bob left")
	})
	t.Run("giver demoted to read", func(t *testing.T) {
		d, key := setup(t)
		if err := d.srv.Projects.SetPerm(d.p.ID, "bob@x.io", PermRead); err != nil {
			t.Fatal(err)
		}
		refused(t, d, key, "bob can only read")
	})
	t.Run("giver shut out of a folder", func(t *testing.T) {
		d, key := setup(t)
		d.send("PUT", "/api/p/"+d.p.ID+"/folders", `{"prefix":"konny","perms":{"bob@x.io":"none"}}`, "alice", nil, 200)
		paths := keyPaths(t, d, d.dataQuery(), key)
		if slices.ContainsFunc(paths, func(p string) bool { return strings.HasPrefix(p, "konny/") }) ||
			!slices.Contains(paths, "acme/placements/editorial/d.com.json") {
			t.Fatalf("with bob shut out of konny/, the app he set up sees %v", paths)
		}
	})
	t.Run("giver shut out of the folder of a folder grant", func(t *testing.T) {
		d := writerHub(t)
		app, principal := newApp(d, d.p.Org, "bob", "C")
		key := keyAs(d, app, "alice")
		d.grant("bob", principal, `{"level":"read","path":"acme"}`, 200)
		if paths := keyPaths(t, d, d.dataQuery(), key); !under(paths, "acme/") {
			t.Fatalf("folder grant: %v", paths)
		}
		// An admin edits the rule, keeping the app's grant as it was: it
		// stays bob's, and so it ends with bob's access there.
		d.send("PUT", "/api/p/"+d.p.ID+"/folders",
			`{"prefix":"acme","perms":{"bob@x.io":"none","`+principal+`":"read"}}`, "alice", nil, 200)
		refused(t, d, key, "bob shut out of acme/")
	})
	t.Run("an owner's grant stands when a writer leaves", func(t *testing.T) {
		d := writerHub(t)
		app, principal := newApp(d, d.p.Org, "bob", "D")
		key := keyAs(d, app, "alice")
		d.grant("alice", principal, `{"level":"read"}`, 200)
		d.send("DELETE", "/api/orgs/"+d.p.Org+"/members/bob@x.io", "", "alice", nil, 200)
		if paths := keyPaths(t, d, d.dataQuery(), key); len(paths) == 0 {
			t.Fatal("an owner's grant stopped when the app's creator left")
		}
	})
	t.Run("the giver leaves through another hub process", func(t *testing.T) {
		d, key := setup(t)
		other, err := NewOrgDB(d.srv.Dir.(LocalDirectory).OrgDB.repo)
		if err != nil {
			t.Fatal(err)
		}
		if err := other.RemoveMember(d.p.Org, "bob@x.io"); err != nil {
			t.Fatal(err)
		}
		refused(t, d, key, "bob removed on another process")
	})
}

// A key works only while the member who minted it could still mint it.
func TestAnAppKeyStopsWhenItsMinterLeaves(t *testing.T) {
	d := writerHub(t)
	app, principal := newApp(d, d.p.Org, "bob", "B")
	bobs := keyAs(d, app, "bob")
	alices := keyAs(d, app, "alice")
	d.grant("alice", principal, `{"level":"read"}`, 200)
	d.send("DELETE", "/api/orgs/"+d.p.Org+"/members/bob@x.io", "", "alice", nil, 200)
	if rec := keyCall(d, "POST", d.dataQuery(), `{"pattern":"**/*.json"}`, bobs); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a key minted by a member who left: %d", rec.Code)
	}
	if rec := keyCall(d, "POST", d.dataQuery(), `{"pattern":"**/*.json"}`, alices); rec.Code != 200 {
		t.Fatalf("an owner's key for the same app: %d %s", rec.Code, rec.Body)
	}
}

// A member who can write turns Data on and off; one who can only read cannot.
func TestDataSettingNeedsWrite(t *testing.T) {
	x, err := OpenDataIndex("sqlite", filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	d := newDataHub(t, x)
	if err := d.srv.Projects.SetPerm(d.p.ID, "carol@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	url := "/api/p/" + d.p.ID + "/data"
	d.send("PUT", url, `{"enabled":true}`, "carol", nil, http.StatusForbidden)
	d.send("PUT", url, `{"enabled":true}`, "dave", nil, http.StatusForbidden)
	d.send("PUT", url, `{"enabled":true}`, "bob", nil, http.StatusOK)
	if p, _ := d.srv.Projects.Get(d.p.ID); !p.Data {
		t.Fatal("a writer's PUT did not turn Data on")
	}
	d.send("PUT", url, `{"enabled":false}`, "carol", nil, http.StatusForbidden)
	d.send("PUT", url, `{"enabled":false}`, "bob", nil, http.StatusOK)
	if p, _ := d.srv.Projects.Get(d.p.ID); p.Data {
		t.Fatal("a writer's PUT did not turn Data off")
	}
}

// Who gave a grant and who minted a key survive a reload on every backend.
func TestGrantAndKeyAttributionPersists(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)
			s := be.open(t)
			defer s.Close()
			db, err := NewProjectDB(s.Projects())
			if err != nil {
				t.Fatal(err)
			}
			p, _, err := db.GetOrCreate("w-"+be.name, "org1")
			if err != nil {
				t.Fatal(err)
			}
			if err := db.SetPermBy(p.ID, "app:a1", PermRead, "bob@x.io"); err != nil {
				t.Fatal(err)
			}
			if err := db.SetFolderGrant(p.ID, "docs/", "app:a1", PermRead, "carol@x.io"); err != nil {
				t.Fatal(err)
			}
			apps, err := NewAppDB(s.Apps())
			if err != nil {
				t.Fatal(err)
			}
			a, err := apps.Create("org1", "A", "bob@x.io")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := apps.Mint(a.ID, "bob@x.io"); err != nil {
				t.Fatal(err)
			}

			fresh, err := NewProjectDB(s.Projects())
			if err != nil {
				t.Fatal(err)
			}
			got, _ := fresh.Get(p.ID)
			if got.GrantedBy["app:a1"] != "bob@x.io" || len(got.Folders) != 1 ||
				got.Folders[0].Prefix != "docs/" || got.Folders[0].Default != "" ||
				got.Folders[0].GrantedBy["app:a1"] != "carol@x.io" {
				t.Fatalf("after a reload: %+v", got)
			}
			freshApps, err := NewAppDB(s.Apps())
			if err != nil {
				t.Fatal(err)
			}
			if _, keys := freshApps.List("org1"); len(keys[a.ID]) != 1 || keys[a.ID][0].MintedBy != "bob@x.io" {
				t.Fatalf("keys after a reload: %+v", keys)
			}

			if err := db.DropFolderGrant(p.ID, "docs/", "app:a1"); err != nil {
				t.Fatal(err)
			}
			fresh, _ = NewProjectDB(s.Projects())
			if got, _ := fresh.Get(p.ID); len(got.Folders) != 1 || len(got.Folders[0].Perms) != 0 {
				t.Fatalf("after dropping the folder grant: %+v", got.Folders)
			}
		})
	}
}

// granted_by and minted_by are guarded: re-adding one to a table that already
// holds rows would make every grant and key it held count again, so a store at
// the schema that introduced them refuses, while an older store gains them.
func TestMigrateNeverRevivesUnattributedGrants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	s, err := OpenSQLStore("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := NewProjectDB(s.Projects())
	p, _, _ := db.GetOrCreate("w", "org1")
	if err := db.SetPermBy(p.ID, "app:a1", PermRead, "bob@x.io"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`ALTER TABLE project_perms DROP COLUMN granted_by`); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSQLStore("sqlite", path); err == nil || !strings.Contains(err.Error(), "granted_by") {
		t.Fatalf("a store at schema 3 missing granted_by opened: %v", err)
	}
	if _, err := raw.Exec(`UPDATE schema_meta SET value = '2' WHERE key = 'version'`); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLStore("sqlite", path)
	if err != nil {
		t.Fatalf("a schema 2 store did not gain granted_by: %v", err)
	}
	defer s.Close()
	db, _ = NewProjectDB(s.Projects())
	if got, _ := db.Get(p.ID); got.Perms["app:a1"] != PermRead || got.GrantedBy["app:a1"] != "" {
		t.Fatalf("an older store's grant after the column: %+v", got)
	}
}
