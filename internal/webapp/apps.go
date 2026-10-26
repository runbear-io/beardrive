package webapp

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

/* Apps are principals for servers: SearchD reading a project's data is not a
   person, and should not hold a person's credential.

   An app belongs to one organization, is created there by an owner, and is
   granted access to a project the way a member is — through the project's
   permissions (or a folder rule) under its principal, app:<id> — except that
   it gets nothing it was not explicitly granted: no project default, never
   admin. It authenticates with keys the hub issues (bda_…), stored as digests,
   several at a time so one can replace another without a gap. A key opens the
   project read routes for data and history, and nothing else: no tree, no
   files, no writes, no org or account surface.

   Apps live in the hub's own metadata, not in the AuthProvider's directory,
   so they work the same under any provider. */

// App is one app principal.
type App struct {
	ID      string    `json:"id"`
	Org     string    `json:"org"`
	Name    string    `json:"name"`
	Creator string    `json:"creator,omitempty"`
	Created time.Time `json:"created"`
}

// AppKey is one of an app's keys, by digest. Hint is the last four characters
// of the key, so its owner can tell two keys apart.
type AppKey struct {
	ID       string    `json:"id"`
	App      string    `json:"app"`
	Digest   string    `json:"digest"`
	Hint     string    `json:"hint"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitzero"`
}

// RoleApp is an app's role in its own organization: it can hold explicit
// grants there and nothing else.
const RoleApp = "app"

const (
	appKeyPrefix   = "bda_"
	appPrincipalNS = "app:"
	// appTouchEvery bounds how often a key's last use is written: often enough
	// to tell a live key from a dead one, rarely enough not to write per request.
	appTouchEvery = time.Minute
)

// appPrincipal is the key an app holds grants under, where an account would
// use its email.
func appPrincipal(id string) string { return appPrincipalNS + id }

func isAppPrincipal(principal string) bool { return strings.HasPrefix(principal, appPrincipalNS) }

// AppDB is the app registry: in memory, re-read when the store has moved, so
// a key revoked by another hub process stops working here too.
type AppDB struct {
	repo   AppRepo
	mu     sync.Mutex
	ver    versionGate
	warned bool
	apps   map[string]App
	keys   map[string]AppKey
	digest map[string]string // digest → key id
}

func NewAppDB(repo AppRepo) (*AppDB, error) {
	db := &AppDB{repo: repo}
	apps, keys, err := repo.Load()
	if err != nil {
		return nil, err
	}
	db.install(apps, keys)
	return db, nil
}

// OpenAppDB loads the file-backed registry at path.
func OpenAppDB(path string) (*AppDB, error) { return NewAppDB(newFileAppRepo(path)) }

func (db *AppDB) install(apps []App, keys []AppKey) {
	db.apps, db.keys, db.digest = map[string]App{}, map[string]AppKey{}, map[string]string{}
	for _, a := range apps {
		db.apps[a.ID] = a
	}
	for _, k := range keys {
		db.keys[k.ID] = k
		db.digest[k.Digest] = k.ID
	}
}

// refresh re-reads the registry when the store has moved. Callers hold mu.
func (db *AppDB) refresh() {
	token, stale := db.ver.stale(db.repo, &db.mu)
	if !stale {
		return
	}
	apps, keys, err := db.repo.Load()
	if err != nil {
		if !db.warned {
			db.warned = true
			log.Printf("beardrive: app registry re-read failed, serving the last known keys: %v", err)
		}
		return
	}
	db.warned = false
	db.ver.fresh(token)
	db.install(apps, keys)
}

// Get returns an app.
func (db *AppDB) Get(id string) (App, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	a, ok := db.apps[id]
	return a, ok
}

// List returns an organization's apps and each app's keys, oldest first.
func (db *AppDB) List(org string) ([]App, map[string][]AppKey) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	var apps []App
	keys := map[string][]AppKey{}
	for _, a := range db.apps {
		if a.Org == org {
			apps = append(apps, a)
		}
	}
	for _, k := range db.keys {
		if a, ok := db.apps[k.App]; ok && a.Org == org {
			keys[k.App] = append(keys[k.App], k)
		}
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Created.Before(apps[j].Created) })
	for _, ks := range keys {
		sort.Slice(ks, func(i, j int) bool { return ks[i].Created.Before(ks[j].Created) })
	}
	return apps, keys
}

func (db *AppDB) Create(org, name, creator string) (App, error) {
	a := App{ID: "a" + randHex(8), Org: org, Name: name, Creator: creator, Created: time.Now().UTC()}
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.repo.PutApp(a); err != nil {
		return App{}, err
	}
	db.apps[a.ID] = a
	return a, nil
}

// Delete removes an app and every key it holds.
func (db *AppDB) Delete(id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.repo.DeleteApp(id); err != nil {
		return err
	}
	delete(db.apps, id)
	for kid, k := range db.keys {
		if k.App == id {
			delete(db.keys, kid)
			delete(db.digest, k.Digest)
		}
	}
	return nil
}

// Mint issues a key and returns its plaintext — the only time it exists.
func (db *AppDB) Mint(app string) (string, AppKey, error) {
	token := appKeyPrefix + randHex(24)
	k := AppKey{ID: "k" + randHex(8), App: app, Digest: hashToken(token), Hint: token[len(token)-4:], Created: time.Now().UTC()}
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.repo.PutKey(k); err != nil {
		return "", AppKey{}, err
	}
	db.keys[k.ID] = k
	db.digest[k.Digest] = k.ID
	return token, k, nil
}

// Revoke deletes one key of an app.
func (db *AppDB) Revoke(app, id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	k, ok := db.keys[id]
	if !ok || k.App != app {
		return fmt.Errorf("no such key")
	}
	if err := db.repo.DeleteKey(id); err != nil {
		return err
	}
	delete(db.keys, id)
	delete(db.digest, k.Digest)
	return nil
}

// Authenticate resolves a key to its app, re-reading the registry first so a
// revocation anywhere is honoured, and records the use at most once a minute.
func (db *AppDB) Authenticate(token string) (App, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	id, ok := db.digest[hashToken(token)]
	if !ok {
		return App{}, false
	}
	k := db.keys[id]
	a, ok := db.apps[k.App]
	if !ok {
		return App{}, false
	}
	if now := time.Now().UTC(); now.Sub(k.LastUsed) >= appTouchEvery {
		k.LastUsed = now
		if err := db.repo.TouchKey(id, now); err == nil {
			db.keys[id] = k
		}
	}
	return a, true
}

// ---- requests ----

// appRoutes is everything a key can reach: a project's data and its history.
var appRoutes = []struct {
	method string
	path   *regexp.Regexp
}{
	{http.MethodPost, regexp.MustCompile(`^/api/p/[^/]+/data/(query|describe)$`)},
	{http.MethodGet, regexp.MustCompile(`^/api/p/[^/]+/history(/summary|/emptied)?$`)},
}

func appRoute(r *http.Request) bool {
	for _, rt := range appRoutes {
		if r.Method == rt.method && rt.path.MatchString(r.URL.Path) {
			return true
		}
	}
	return false
}

// appUser resolves a request carrying an app key, answering it when the key is
// unknown or the route is not one a key may reach. ok=false with handled=false
// means the request carries no app key at all.
func (s *Server) appUser(w http.ResponseWriter, r *http.Request) (u User, handled, ok bool) {
	tok, isBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !isBearer || !strings.HasPrefix(tok, appKeyPrefix) {
		return User{}, false, false
	}
	var a App
	if s.Apps != nil {
		a, ok = s.Apps.Authenticate(tok)
	}
	if !ok {
		http.Error(w, "this app key is not valid; it may have been revoked", http.StatusUnauthorized)
		return User{}, true, false
	}
	if !appRoute(r) {
		http.Error(w, "an app key can only query a project's data and read its history", http.StatusForbidden)
		return User{}, true, false
	}
	principal := appPrincipal(a.ID)
	return User{ID: principal, Email: principal, Name: a.Name}, true, true
}

// roleIn is a principal's role in an organization: the directory's answer for
// an account, and RoleApp for one of the organization's own apps.
func (s *Server) roleIn(org, principal string) string {
	if isAppPrincipal(principal) {
		if s.Apps == nil {
			return ""
		}
		if a, ok := s.Apps.Get(strings.TrimPrefix(principal, appPrincipalNS)); ok && a.Org == org {
			return RoleApp
		}
		return ""
	}
	return s.Dir.Role(org, principal)
}

// ---- administration: org owners ----

// appOwner answers unless the caller owns the organization. Apps are an
// organization's credentials to its projects, so creating one, minting its
// keys and revoking them is an owner's job, as invites are.
func (s *Server) appOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.Apps == nil || s.Dir == nil {
		http.Error(w, "apps are not enabled on this hub", http.StatusNotFound)
		return "", false
	}
	org := r.PathValue("org")
	if s.Dir.Role(org, normEmail(s.requestUser(r).Email)) != RoleOwner {
		http.Error(w, "only an owner of this organization can manage its apps", http.StatusForbidden)
		return "", false
	}
	return org, true
}

// appOf resolves the {app} of the path inside the caller's organization.
func (s *Server) appOf(w http.ResponseWriter, r *http.Request, org string) (App, bool) {
	a, ok := s.Apps.Get(r.PathValue("app"))
	if !ok || a.Org != org {
		http.Error(w, "no such app", http.StatusNotFound)
		return App{}, false
	}
	return a, true
}

func (s *Server) handleAppList(w http.ResponseWriter, r *http.Request) {
	org, ok := s.appOwner(w, r)
	if !ok {
		return
	}
	apps, keys := s.Apps.List(org)
	out := []map[string]any{}
	for _, a := range apps {
		// Never the digest: it is as good as the key to anyone who can try
		// it against a copy of the store.
		ks := []map[string]any{}
		for _, k := range keys[a.ID] {
			row := map[string]any{"id": k.ID, "hint": k.Hint, "created": k.Created}
			if !k.LastUsed.IsZero() {
				row["last_used"] = k.LastUsed
			}
			ks = append(ks, row)
		}
		out = append(out, map[string]any{
			"id": a.ID, "name": a.Name, "principal": appPrincipal(a.ID),
			"creator": a.Creator, "created": a.Created, "keys": ks,
		})
	}
	writeJSON(w, map[string]any{"apps": out})
}

func (s *Server) handleAppCreate(w http.ResponseWriter, r *http.Request) {
	org, ok := s.appOwner(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req)
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 80 {
		http.Error(w, "give the app a name of at most 80 characters", http.StatusBadRequest)
		return
	}
	a, err := s.Apps.Create(org, name, normEmail(s.requestUser(r).Email))
	if err != nil {
		storageErr(w, http.StatusBadGateway, "could not create the app", err)
		return
	}
	writeJSON(w, map[string]any{"id": a.ID, "name": a.Name, "principal": appPrincipal(a.ID)})
}

func (s *Server) handleAppDelete(w http.ResponseWriter, r *http.Request) {
	org, ok := s.appOwner(w, r)
	if !ok {
		return
	}
	a, ok := s.appOf(w, r, org)
	if !ok {
		return
	}
	if err := s.Apps.Delete(a.ID); err != nil {
		storageErr(w, http.StatusBadGateway, "could not delete the app", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAppKeyCreate(w http.ResponseWriter, r *http.Request) {
	org, ok := s.appOwner(w, r)
	if !ok {
		return
	}
	a, ok := s.appOf(w, r, org)
	if !ok {
		return
	}
	token, k, err := s.Apps.Mint(a.ID)
	if err != nil {
		storageErr(w, http.StatusBadGateway, "could not create the key", err)
		return
	}
	writeJSON(w, map[string]any{"key": token, "id": k.ID, "hint": k.Hint})
}

func (s *Server) handleAppKeyRevoke(w http.ResponseWriter, r *http.Request) {
	org, ok := s.appOwner(w, r)
	if !ok {
		return
	}
	a, ok := s.appOf(w, r, org)
	if !ok {
		return
	}
	if err := s.Apps.Revoke(a.ID, r.PathValue("key")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
