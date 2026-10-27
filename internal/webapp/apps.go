package webapp

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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
	// MintedBy is the member who minted the key. It works only while they
	// could still mint it (Server.keyStands); empty on a key minted before
	// keys were attributed, which only an owner could do.
	MintedBy string `json:"minted_by,omitempty"`
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
func (db *AppDB) Mint(app, by string) (string, AppKey, error) {
	token := appKeyPrefix + randHex(24)
	k := AppKey{
		ID: "k" + randHex(8), App: app, Digest: hashToken(token), Hint: token[len(token)-4:],
		Created: time.Now().UTC(), MintedBy: by,
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.repo.PutKey(k); err != nil {
		return "", AppKey{}, err
	}
	db.keys[k.ID] = k
	db.digest[k.Digest] = k.ID
	return token, k, nil
}

// errNoSuchKey is Revoke's answer for a key that is not the app's: a 404, where
// a store failure is a 502 that never shows the store's own message.
var errNoSuchKey = errors.New("no such key")

// Revoke deletes one key of an app.
func (db *AppDB) Revoke(app, id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	k, ok := db.keys[id]
	if !ok || k.App != app {
		return errNoSuchKey
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
func (db *AppDB) Authenticate(token string) (App, AppKey, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	id, ok := db.digest[hashToken(token)]
	if !ok {
		return App{}, AppKey{}, false
	}
	k := db.keys[id]
	a, ok := db.apps[k.App]
	if !ok {
		return App{}, AppKey{}, false
	}
	if now := time.Now().UTC(); now.Sub(k.LastUsed) >= appTouchEvery {
		k.LastUsed = now
		if err := db.repo.TouchKey(id, now); err == nil {
			db.keys[id] = k
		}
	}
	return a, k, true
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
	var k AppKey
	if s.Apps != nil {
		a, k, ok = s.Apps.Authenticate(tok)
	}
	if !ok || !s.keyStands(a, k) {
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

// keyStands says whether a key still works: only while the member who minted
// it could still mint it, judged on every request. So the keys of a member
// who left the organization, or of an app's creator who left it, stop at
// once, with nothing to clean up.
func (s *Server) keyStands(a App, k AppKey) bool {
	if k.MintedBy == "" || s.Dir == nil {
		return true // minted before keys were attributed: by an owner
	}
	return manages(a, k.MintedBy, s.Dir.Role(a.Org, k.MintedBy))
}

// manages says whether a member with role in an app's organization may mint
// and revoke its keys and delete it: an owner, or the member who created it.
func manages(a App, email, role string) bool {
	switch role {
	case RoleOwner:
		return true
	case "", RoleApp:
		return false
	}
	return email != "" && normEmail(a.Creator) == email
}

// ---- administration: org members ----

// appMember answers unless the caller is a member of the organization. Any
// member may create an app: a scoped, read-only key is a safer credential
// than the device token every member can already mint for themselves.
func (s *Server) appMember(w http.ResponseWriter, r *http.Request) (org, me, role string, ok bool) {
	if s.Apps == nil || s.Dir == nil {
		http.Error(w, "apps are not enabled on this hub", http.StatusNotFound)
		return "", "", "", false
	}
	org = r.PathValue("org")
	me = normEmail(s.requestUser(r).Email)
	role = s.Dir.Role(org, me)
	if role == "" || role == RoleApp {
		http.Error(w, "only a member of this organization can use its apps", http.StatusForbidden)
		return "", "", "", false
	}
	return org, me, role, true
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

// appManaged resolves the {app} of the path and answers unless the caller
// manages it.
func (s *Server) appManaged(w http.ResponseWriter, r *http.Request) (App, string, bool) {
	org, me, role, ok := s.appMember(w, r)
	if !ok {
		return App{}, "", false
	}
	a, ok := s.appOf(w, r, org)
	if !ok {
		return App{}, "", false
	}
	if !manages(a, me, role) {
		http.Error(w, "only the member who created this app, or an owner, can manage it", http.StatusForbidden)
		return App{}, "", false
	}
	return a, me, true
}

func (s *Server) handleAppList(w http.ResponseWriter, r *http.Request) {
	org, me, role, ok := s.appMember(w, r)
	if !ok {
		return
	}
	apps, keys := s.Apps.List(org)
	out := []map[string]any{}
	for _, a := range apps {
		// Every member sees the org's apps, so they can grant one read; only
		// the apps they manage come with their keys.
		mine := manages(a, me, role)
		// Never the digest: it is as good as the key to anyone who can try
		// it against a copy of the store.
		ks := []map[string]any{}
		for _, k := range keys[a.ID] {
			if !mine {
				break
			}
			row := map[string]any{"id": k.ID, "hint": k.Hint, "created": k.Created}
			if !k.LastUsed.IsZero() {
				row["last_used"] = k.LastUsed
			}
			ks = append(ks, row)
		}
		out = append(out, map[string]any{
			"id": a.ID, "name": a.Name, "principal": appPrincipal(a.ID),
			"creator": a.Creator, "created": a.Created, "manage": mine, "keys": ks,
		})
	}
	writeJSON(w, map[string]any{"apps": out})
}

func (s *Server) handleAppCreate(w http.ResponseWriter, r *http.Request) {
	org, me, _, ok := s.appMember(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req)
	// The same label rules as a project's name: no control, format or bidi
	// characters, which would reorder a row of the owner's admin table.
	name := strings.TrimSpace(trimText(req.Name, 81))
	if name == "" || utf8.RuneCountInString(name) > 80 {
		http.Error(w, "give the app a name of at most 80 characters", http.StatusBadRequest)
		return
	}
	a, err := s.Apps.Create(org, name, me)
	if err != nil {
		storageErr(w, http.StatusBadGateway, "could not create the app", err)
		return
	}
	writeJSON(w, map[string]any{"id": a.ID, "name": a.Name, "principal": appPrincipal(a.ID)})
}

func (s *Server) handleAppDelete(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.appManaged(w, r)
	if !ok {
		return
	}
	if err := s.Apps.Delete(a.ID); err != nil {
		storageErr(w, http.StatusBadGateway, "could not delete the app", err)
		return
	}
	s.dropAppGrants(a)
	writeJSON(w, map[string]any{"ok": true})
}

// dropAppGrants removes a deleted app's grants from its org's projects and
// folder rules. They confer nothing once the app is gone (roleIn), but they
// would stay on every People list and rule, naming an app that no longer
// exists. A failure is logged and left: the grant is already inert.
func (s *Server) dropAppGrants(a App) {
	if s.Projects == nil {
		return
	}
	principal := appPrincipal(a.ID)
	for _, p := range s.Projects.List() {
		if p.Org != a.Org {
			continue
		}
		if err := s.Projects.dropPerm(p.ID, principal); err != nil {
			log.Printf("beardrive: delete app %s: project %s grant: %v", a.ID, p.ID, err)
		}
		// Grant by grant, never rule by rule: rewriting a rule from this
		// listing would put back one another process has just removed.
		if err := s.Projects.dropFolderGrants(p.ID, principal); err != nil {
			log.Printf("beardrive: delete app %s: project %s folder grants: %v", a.ID, p.ID, err)
		}
		if err := s.Projects.dropAppFolders(p.ID, principal); err != nil {
			log.Printf("beardrive: delete app %s: project %s app folder grants: %v", a.ID, p.ID, err)
		}
	}
}

func (s *Server) handleAppKeyCreate(w http.ResponseWriter, r *http.Request) {
	a, me, ok := s.appManaged(w, r)
	if !ok {
		return
	}
	token, k, err := s.Apps.Mint(a.ID, me)
	if err != nil {
		storageErr(w, http.StatusBadGateway, "could not create the key", err)
		return
	}
	writeJSON(w, map[string]any{"key": token, "id": k.ID, "hint": k.Hint})
}

func (s *Server) handleAppKeyRevoke(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.appManaged(w, r)
	if !ok {
		return
	}
	if err := s.Apps.Revoke(a.ID, r.PathValue("key")); errors.Is(err, errNoSuchKey) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	} else if err != nil {
		storageErr(w, http.StatusBadGateway, "could not revoke the key", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
