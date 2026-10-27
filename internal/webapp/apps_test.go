package webapp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// keyCall sends a request with an app key and nothing else.
func keyCall(d *dataHub, method, url, body, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	d.h.ServeHTTP(rec, req)
	return rec
}

func newApp(d *dataHub, org, who, name string) (id, principal string) {
	d.t.Helper()
	rec := d.send("POST", "/api/orgs/"+org+"/apps", `{"name":"`+name+`"}`, who, nil, 200)
	var out struct{ ID, Principal string }
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out.ID, out.Principal
}

func mintKey(d *dataHub, org, app string) string {
	d.t.Helper()
	rec := d.send("POST", "/api/orgs/"+org+"/apps/"+app+"/keys", "", "alice", nil, 200)
	var out struct{ Key string }
	json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.HasPrefix(out.Key, appKeyPrefix) {
		d.t.Fatalf("minted %q", out.Key)
	}
	return out.Key
}

func TestAppKeyReadsOnlyWhatItIsGranted(t *testing.T) {
	x, err := OpenDataIndex("sqlite", filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	d := newDataHub(t, x)
	apps, err := OpenAppDB(filepath.Join(t.TempDir(), "apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	d.srv.Apps = apps
	reads, err := OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	d.srv.Reads = reads
	d.enable()
	org, pid := d.p.Org, d.p.ID
	query := "/api/p/" + pid + "/data/query"

	// Only an owner manages apps.
	d.send("POST", "/api/orgs/"+org+"/apps", `{"name":"x"}`, "bob", nil, http.StatusForbidden)
	d.send("GET", "/api/orgs/"+org+"/apps", "", "bob", nil, http.StatusForbidden)
	app, principal := newApp(d, org, "alice", "SearchD")
	key := mintKey(d, org, app)

	// No grant, no access: an app does not inherit the project default
	// (write, for members) the way an org member does.
	if rec := keyCall(d, "POST", query, `{"pattern":"**/*.json"}`, key); rec.Code != http.StatusForbidden {
		t.Fatalf("an ungranted app queried: %d %s", rec.Code, rec.Body)
	}
	// Nor does a folder rule alone: a rule narrows or widens a project the
	// app can already see, and never opens one.
	d.send("PUT", "/api/p/"+pid+"/folders", `{"prefix":"konny","perms":{"`+principal+`":"read"}}`, "alice", nil, 200)
	if rec := keyCall(d, "POST", query, `{"pattern":"konny/**/*.json"}`, key); rec.Code != http.StatusForbidden {
		t.Fatalf("a folder rule alone opened the project to an app: %d %s", rec.Code, rec.Body)
	}
	d.send("PUT", "/api/p/"+pid+"/permissions/"+principal, `{"level":"admin"}`, "alice", nil, http.StatusBadRequest)
	d.send("PUT", "/api/p/"+pid+"/permissions/"+principal, `{"level":"read"}`, "alice", nil, 200)
	rec := keyCall(d, "POST", query, `{"pattern":"**/*.json","select":["$doc"]}`, key)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "secret/") {
		t.Fatalf("granted app query: %d %s", rec.Code, rec.Body)
	}
	if got := reads.Heat(pid, "", time.Time{}); len(got) != 0 {
		t.Fatalf("an app's reads counted as heat: %v", got)
	}

	// A folder rule can name the app like any member.
	d.send("PUT", "/api/p/"+pid+"/folders", `{"prefix":"secret","default":"none","perms":{"`+principal+`":"read"}}`, "alice", nil, 200)
	if rec := keyCall(d, "POST", query, `{"pattern":"secret/*.json"}`, key); !strings.Contains(rec.Body.String(), "secret/s.json") {
		t.Fatalf("a folder grant to the app did not open the folder: %s", rec.Body)
	}

	// A key reaches data and history, and nothing else.
	for _, h := range []string{"/history?prefix=konny/", "/history/summary", "/history/emptied"} {
		if rec := keyCall(d, "GET", "/api/p/"+pid+h, "", key); rec.Code != 200 {
			t.Fatalf("%s with a key: %d %s", h, rec.Code, rec.Body)
		}
	}
	for _, c := range []struct{ method, url string }{
		{"GET", "/api/p/" + pid + "/tree"},
		{"GET", "/api/p/" + pid + "/file?path=notes/plan.md"},
		{"PUT", "/api/p/" + pid + "/upload/content?path=x.json"},
		{"GET", "/api/p/" + pid + "/events"},
		{"GET", "/api/projects"},
		{"GET", "/api/orgs/" + org + "/apps"},
		{"PUT", "/api/p/" + pid + "/permissions/" + principal},
	} {
		if rec := keyCall(d, c.method, c.url, `{}`, key); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s with a key: %d, want 403", c.method, c.url, rec.Code)
		}
	}

	// Several keys at once, each revocable; the list shows use, never digests.
	second := mintKey(d, org, app)
	list := d.send("GET", "/api/orgs/"+org+"/apps", "", "alice", nil, 200).Body.String()
	if strings.Contains(list, hashToken(key)) || !strings.Contains(list, `"last_used":"20`) {
		t.Fatalf("app list: %s", list)
	}
	var out struct {
		Apps []struct {
			Keys []struct{ ID, Hint string }
		}
	}
	json.Unmarshal([]byte(list), &out)
	var first string
	for _, k := range out.Apps[0].Keys {
		if k.Hint == key[len(key)-4:] {
			first = k.ID
		}
	}
	d.send("DELETE", "/api/orgs/"+org+"/apps/"+app+"/keys/"+first, "", "alice", nil, 200)
	if rec := keyCall(d, "POST", query, `{"pattern":"**/*.json","count":true}`, key); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked key: %d", rec.Code)
	}
	if rec := keyCall(d, "POST", query, `{"pattern":"**/*.json","count":true}`, second); rec.Code != 200 {
		t.Fatalf("the other key after one was revoked: %d %s", rec.Code, rec.Body)
	}
	d.send("DELETE", "/api/orgs/"+org+"/apps/"+app, "", "alice", nil, 200)
	if rec := keyCall(d, "POST", query, `{"pattern":"**/*.json","count":true}`, second); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a deleted app's key: %d", rec.Code)
	}
	// Its grants went with it, from the project and from every folder rule.
	p, _ := d.srv.Projects.Get(pid)
	if _, has := p.Perms[principal]; has {
		t.Fatalf("a deleted app keeps its project grant: %v", p.Perms)
	}
	for _, rule := range p.Folders {
		if _, has := rule.Perms[principal]; has {
			t.Fatalf("a deleted app keeps its grant on %s: %v", rule.Prefix, rule.Perms)
		}
	}

	// Another organization's app cannot be granted here.
	other, err := d.srv.Dir.Create("elsewhere", "dave@x.io")
	if err != nil {
		t.Fatal(err)
	}
	_, theirs := newApp(d, other.ID, "dave", "Theirs")
	d.send("PUT", "/api/p/"+pid+"/permissions/"+theirs, `{"level":"read"}`, "alice", nil, http.StatusBadRequest)
}

// Every metadata backend keeps apps and keys the same way.
func TestAppsConformance(t *testing.T) {
	for _, be := range metaBackends(t) {
		name := be.name
		t.Run(name, func(t *testing.T) {
			be.reset(t)
			s := be.open(t)
			defer s.Close()
			db, err := NewAppDB(s.Apps())
			if err != nil {
				t.Fatal(err)
			}
			a, err := db.Create("org-"+name, "SearchD", "alice@x.io")
			if err != nil {
				t.Fatal(err)
			}
			tok, k, err := db.Mint(a.ID)
			if err != nil {
				t.Fatal(err)
			}
			// A second registry over the same store sees the key, its use and
			// its revocation: what another hub process would.
			other, err := NewAppDB(s.Apps())
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := other.Authenticate(tok); !ok || got.ID != a.ID {
				t.Fatalf("the other registry does not know the key")
			}
			if _, keys := db.List("org-" + name); len(keys[a.ID]) != 1 || keys[a.ID][0].LastUsed.IsZero() {
				t.Fatalf("keys after use: %+v", keys)
			}
			if err := db.Revoke(a.ID, k.ID); err != nil {
				t.Fatal(err)
			}
			if _, ok := other.Authenticate(tok); ok {
				t.Fatal("a key revoked by one registry still works in the other")
			}
			// A use recorded after the revocation, by a process that resolved
			// the key just before it, does not write the key back.
			if err := s.Apps().TouchKey(k.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, left, err := s.Apps().Load(); err != nil || len(left) != 0 {
				t.Fatalf("a use after the revocation wrote the key back: %+v %v", left, err)
			}
			tok2, _, _ := db.Mint(a.ID)
			if err := db.Delete(a.ID); err != nil {
				t.Fatal(err)
			}
			if _, ok := other.Authenticate(tok2); ok {
				t.Fatal("a deleted app's key still works in the other registry")
			}
		})
	}
}

// failingKeyDelete is an app store whose key deletion fails the way a database
// does, with a message that names its own host.
type failingKeyDelete struct{ AppRepo }

func (failingKeyDelete) DeleteKey(string) error {
	return errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")
}

// A failed revocation is the store's failure, not a missing key: a 502 that
// does not repeat the store's message, while an unknown key stays a 404.
// And an app's name keeps to the same label rules as a project's.
func TestAppKeyRevokeAndNameEdges(t *testing.T) {
	x, err := OpenDataIndex("sqlite", filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	d := newDataHub(t, x)
	apps, err := NewAppDB(failingKeyDelete{newFileAppRepo(filepath.Join(t.TempDir(), "apps.json"))})
	if err != nil {
		t.Fatal(err)
	}
	d.srv.Apps = apps
	org := d.p.Org
	app, _ := newApp(d, org, "alice", `Search\u202eD\u0000`) // JSON escapes: a bidi override and a NUL
	if a, _ := apps.Get(app); a.Name != "SearchD" {
		t.Fatalf("stored name %q, want the label without control or bidi characters", a.Name)
	}
	d.send("POST", "/api/orgs/"+org+"/apps", `{"name":"\u202e\u0007"}`, "alice", nil, http.StatusBadRequest)
	mintKey(d, org, app)
	_, keys := apps.List(org)
	rec := d.send("DELETE", "/api/orgs/"+org+"/apps/"+app+"/keys/"+keys[app][0].ID, "", "alice", nil, http.StatusBadGateway)
	if strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatalf("a revocation failure showed the store's message: %s", rec.Body)
	}
	d.send("DELETE", "/api/orgs/"+org+"/apps/"+app+"/keys/knope", "", "alice", nil, http.StatusNotFound)
}

// loadThen is a project store that runs a hook once, right after a load: what
// another hub process does between this one's read and its write.
type loadThen struct {
	*sqlProjectRepo
	after func()
}

func (l *loadThen) Load() ([]Project, error) {
	ps, err := l.sqlProjectRepo.Load()
	if f := l.after; f != nil {
		l.after = nil
		f()
	}
	return ps, err
}

// Deleting an app removes its folder grants one row at a time. A rule another
// process removed in the meantime stays removed: rewriting rules from this
// process's listing used to put it back.
func TestDeletingAnAppNeverPutsBackARemovedRule(t *testing.T) {
	store, err := OpenSQLStore("sqlite", filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	other, err := NewProjectDB(store.Projects())
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := other.GetOrCreate("wiki", "org1")
	if err != nil {
		t.Fatal(err)
	}
	principal := appPrincipal("a1")
	if err := other.SetFolder(p.ID, FolderRule{Prefix: "secret/", Default: PermNone, Perms: map[string]string{principal: PermRead}}); err != nil {
		t.Fatal(err)
	}
	hooked := &loadThen{sqlProjectRepo: store.Projects().(*sqlProjectRepo)}
	mine, err := NewProjectDB(hooked)
	if err != nil {
		t.Fatal(err)
	}
	// A change this process has not read yet, so its next read reloads; the
	// other process removes the rule right after that reload.
	if err := other.SetFolder(p.ID, FolderRule{Prefix: "open/", Default: PermRead}); err != nil {
		t.Fatal(err)
	}
	hooked.after = func() {
		if err := other.ClearFolder(p.ID, "secret/"); err != nil {
			t.Error(err)
		}
	}
	(&Server{Projects: mine}).dropAppGrants(App{ID: "a1", Org: "org1"})
	fresh, err := NewProjectDB(store.Projects())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := fresh.Get(p.ID)
	for _, rule := range got.Folders {
		if rule.Prefix == "secret/" {
			t.Fatalf("a removed rule came back: %+v", rule)
		}
	}
}
