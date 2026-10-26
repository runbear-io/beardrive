package webapp

import (
	"encoding/json"
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
