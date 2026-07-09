package webapp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Organizations group accounts and own projects: every project belongs to
// exactly one org, and only that org's members can see or sync it. The OSS
// server supports any number of orgs on one hub (a self-hosted deployment is
// typically just one); membership is by account email with two roles. Share
// links (/s/) deliberately stay outside this wall — public is their point.
//
// Same file-backed discipline as the other registries: orgs.json is loaded
// at open and rewritten atomically (temp + rename) on every change.

const (
	RoleOwner  = "owner"
	RoleMember = "member"
)

// Org is one organization.
type Org struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Members map[string]string `json:"members"` // lowercase email → role
	Created time.Time         `json:"created"`
}

// OrgInvite is a mint-once join link. Redeeming it while signed in adds the
// account to the org as a member.
type OrgInvite struct {
	Token   string    `json:"token"`
	Org     string    `json:"org"`
	Creator string    `json:"creator,omitempty"` // account email
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	Uses    int       `json:"uses"` // how many accounts have joined via this link
}

// RecordInviteUse bumps the join counter for an invite (best effort).
func (db *OrgDB) RecordInviteUse(token string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if inv, ok := db.invites[token]; ok {
		inv.Uses++
		db.invites[token] = inv
		db.save()
	}
}

func (i OrgInvite) expired() bool { return time.Now().After(i.Expires) }

// DefaultInviteTTL bounds invite links that don't ask for an expiry.
const DefaultInviteTTL = 7 * 24 * time.Hour

// OrgDB is the file-backed org registry (orgs.json).
type OrgDB struct {
	path string

	mu      sync.Mutex
	byID    map[string]Org
	invites map[string]OrgInvite
}

func OpenOrgDB(path string) (*OrgDB, error) {
	db := &OrgDB{path: path, byID: make(map[string]Org), invites: make(map[string]OrgInvite)}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return db, nil
		}
		return nil, err
	}
	var file struct {
		Orgs    []Org       `json:"orgs"`
		Invites []OrgInvite `json:"invites"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, o := range file.Orgs {
		db.byID[o.ID] = o
	}
	for _, i := range file.Invites {
		db.invites[i.Token] = i
	}
	return db, nil
}

// save persists the registry. Callers hold mu.
func (db *OrgDB) save() error {
	var file struct {
		Orgs    []Org       `json:"orgs"`
		Invites []OrgInvite `json:"invites"`
	}
	for _, o := range db.byID {
		file.Orgs = append(file.Orgs, o)
	}
	sort.Slice(file.Orgs, func(i, j int) bool { return file.Orgs[i].ID < file.Orgs[j].ID })
	for _, i := range db.invites {
		if !i.expired() {
			file.Invites = append(file.Invites, i)
		}
	}
	sort.Slice(file.Invites, func(i, j int) bool { return file.Invites[i].Token < file.Invites[j].Token })
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(db.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(db.path), ".bdrive-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), db.path)
}

func normEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// Create makes a new org owned by ownerEmail.
func (db *OrgDB) Create(name, ownerEmail string) (Org, error) {
	name = trimName(name)
	if name == "" {
		return Org{}, fmt.Errorf("organization name must not be empty")
	}
	o := Org{
		ID: "o-" + randHex(4), Name: name,
		Members: map[string]string{}, Created: time.Now().UTC(),
	}
	if e := normEmail(ownerEmail); e != "" {
		o.Members[e] = RoleOwner
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	db.byID[o.ID] = o
	if err := db.save(); err != nil {
		delete(db.byID, o.ID)
		return Org{}, err
	}
	return o, nil
}

func (db *OrgDB) Get(id string) (Org, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	o, ok := db.byID[id]
	return o, ok
}

// Role returns the account's role in the org, or "" for non-members.
func (db *OrgDB) Role(orgID, email string) string {
	db.mu.Lock()
	defer db.mu.Unlock()
	o, ok := db.byID[orgID]
	if !ok {
		return ""
	}
	return o.Members[normEmail(email)]
}

// OrgsFor returns the orgs the account belongs to, sorted by name.
func (db *OrgDB) OrgsFor(email string) []Org {
	e := normEmail(email)
	db.mu.Lock()
	defer db.mu.Unlock()
	var out []Org
	for _, o := range db.byID {
		if o.Members[e] != "" {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AddMember adds (or keeps) the account in the org with the given role. An
// existing member's role is never downgraded by an invite.
func (db *OrgDB) AddMember(orgID, email, role string) error {
	e := normEmail(email)
	if e == "" {
		return fmt.Errorf("email must not be empty")
	}
	if role != RoleOwner && role != RoleMember {
		return fmt.Errorf("invalid role %q", role)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	o, ok := db.byID[orgID]
	if !ok {
		return fmt.Errorf("no such organization")
	}
	if o.Members[e] == RoleOwner {
		return nil
	}
	o.Members[e] = role
	db.byID[orgID] = o
	return db.save()
}

// RemoveMember drops an account from the org. The last owner cannot be
// removed (an org must always have someone who can administer it).
func (db *OrgDB) RemoveMember(orgID, email string) error {
	e := normEmail(email)
	db.mu.Lock()
	defer db.mu.Unlock()
	o, ok := db.byID[orgID]
	if !ok {
		return fmt.Errorf("no such organization")
	}
	if o.Members[e] == "" {
		return fmt.Errorf("%s is not a member", email)
	}
	if o.Members[e] == RoleOwner && db.ownerCount(o) <= 1 {
		return fmt.Errorf("cannot remove the last owner")
	}
	delete(o.Members, e)
	db.byID[orgID] = o
	return db.save()
}

// SetRole changes an account's role. Demoting the last owner is refused.
func (db *OrgDB) SetRole(orgID, email, role string) error {
	if role != RoleOwner && role != RoleMember {
		return fmt.Errorf("invalid role %q", role)
	}
	e := normEmail(email)
	db.mu.Lock()
	defer db.mu.Unlock()
	o, ok := db.byID[orgID]
	if !ok {
		return fmt.Errorf("no such organization")
	}
	if o.Members[e] == "" {
		return fmt.Errorf("%s is not a member", email)
	}
	if o.Members[e] == RoleOwner && role == RoleMember && db.ownerCount(o) <= 1 {
		return fmt.Errorf("cannot demote the last owner")
	}
	o.Members[e] = role
	db.byID[orgID] = o
	return db.save()
}

// Rename changes the org's display name.
func (db *OrgDB) Rename(orgID, name string) error {
	name = trimName(name)
	if name == "" {
		return fmt.Errorf("organization name must not be empty")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	o, ok := db.byID[orgID]
	if !ok {
		return fmt.Errorf("no such organization")
	}
	o.Name = name
	db.byID[orgID] = o
	return db.save()
}

// ownerCount counts owners in an org. Callers hold mu.
func (db *OrgDB) ownerCount(o Org) int {
	n := 0
	for _, role := range o.Members {
		if role == RoleOwner {
			n++
		}
	}
	return n
}

// ListInvites returns the org's live (non-expired) invites.
func (db *OrgDB) ListInvites(orgID string) []OrgInvite {
	db.mu.Lock()
	defer db.mu.Unlock()
	var out []OrgInvite
	for _, inv := range db.invites {
		if inv.Org == orgID && !inv.expired() {
			out = append(out, inv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// RevokeInvite deletes an invite so its link stops working immediately.
func (db *OrgDB) RevokeInvite(token string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.invites[token]; !ok {
		return false
	}
	delete(db.invites, token)
	db.save()
	return true
}

// CreateInvite mints a join link for the org.
func (db *OrgDB) CreateInvite(orgID, creator string, ttl time.Duration) (OrgInvite, error) {
	if ttl <= 0 {
		ttl = DefaultInviteTTL
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.byID[orgID]; !ok {
		return OrgInvite{}, fmt.Errorf("no such organization")
	}
	inv := OrgInvite{
		Token: randHex(16), Org: orgID, Creator: normEmail(creator),
		Created: time.Now().UTC(), Expires: time.Now().UTC().Add(ttl),
	}
	db.invites[inv.Token] = inv
	if err := db.save(); err != nil {
		delete(db.invites, inv.Token)
		return OrgInvite{}, err
	}
	return inv, nil
}

// Redeem consumes nothing — an invite link can onboard a whole team until it
// expires — it just resolves the token to its live invite.
func (db *OrgDB) Redeem(token string) (OrgInvite, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	inv, ok := db.invites[token]
	if !ok || inv.expired() {
		return OrgInvite{}, false
	}
	return inv, true
}

// ValidInvite reports whether a token is a live invite, without consuming it.
// It lets the signup page permit account creation from an invite link even
// when public self-signup is closed (invite-only hubs).
func (db *OrgDB) ValidInvite(token string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	inv, ok := db.invites[token]
	return ok && !inv.expired()
}

// ---- migration ----

// MigrateOrgs assigns every org-less project to a default org so a hub that
// predates organizations keeps working with zero manual steps. All existing
// accounts join it — they could all see every project before, so anything
// narrower would lock someone out — with the oldest account as owner.
func MigrateOrgs(projects *ProjectDB, orgs *OrgDB, accounts []User) error {
	var orphans []Project
	for _, p := range projects.List() {
		if p.Org == "" {
			orphans = append(orphans, p)
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	owner := ""
	if len(accounts) > 0 {
		owner = accounts[0].Email
	}
	def, err := orgs.Create("default", owner)
	if err != nil {
		return err
	}
	for _, u := range accounts[min(1, len(accounts)):] {
		if err := orgs.AddMember(def.ID, u.Email, RoleMember); err != nil {
			return err
		}
	}
	for _, p := range orphans {
		if err := projects.SetOrg(p.ID, def.ID); err != nil {
			return err
		}
	}
	return nil
}

// ---- HTTP ----

// orgFor resolves a project's org; zero value when orgs are off.
func (s *Server) orgOf(projectID string) string {
	if s.Projects == nil {
		return ""
	}
	p, _ := s.Projects.Get(projectID)
	return p.Org
}

// projectAllowed says whether the request's account may touch the project.
// Without an org registry (single-volume mode, tests, pre-org hubs) every
// authenticated request passes, preserving the old behavior.
func (s *Server) projectAllowed(r *http.Request, projectID string) bool {
	if s.Orgs == nil || s.Auth == nil {
		return true
	}
	org := s.orgOf(projectID)
	if org == "" {
		return true // org-less project (migration happens at startup)
	}
	return s.Orgs.Role(org, s.requestUser(r).Email) != ""
}

// handleOrgList returns the caller's orgs with members (visible to any
// member) and the caller's role.
func (s *Server) handleOrgList(w http.ResponseWriter, r *http.Request) {
	if s.Orgs == nil {
		writeJSON(w, map[string]any{"orgs": []any{}})
		return
	}
	me := s.requestUser(r)
	out := []map[string]any{}
	for _, o := range s.Orgs.OrgsFor(me.Email) {
		members := make([]map[string]string, 0, len(o.Members))
		for email, role := range o.Members {
			members = append(members, map[string]string{"email": email, "role": role})
		}
		sort.Slice(members, func(i, j int) bool { return members[i]["email"] < members[j]["email"] })
		out = append(out, map[string]any{
			"id": o.ID, "name": o.Name, "role": o.Members[normEmail(me.Email)],
			"members": members, "created": o.Created,
		})
	}
	writeJSON(w, map[string]any{"orgs": out})
}

// requireOwner returns true and the caller's email when they own the org;
// otherwise it writes the error response and returns false.
func (s *Server) requireOwner(w http.ResponseWriter, r *http.Request, orgID string) (string, bool) {
	if s.Orgs == nil {
		http.Error(w, "organizations are not enabled on this server", http.StatusNotFound)
		return "", false
	}
	me := s.requestUser(r)
	if s.Orgs.Role(orgID, me.Email) != RoleOwner {
		http.Error(w, "only an organization owner can do that", http.StatusForbidden)
		return "", false
	}
	return normEmail(me.Email), true
}

// handleOrgRename renames the org. Owners only.
func (s *Server) handleOrgRename(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("org")
	if _, ok := s.requireOwner(w, r, orgID); !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Orgs.Rename(orgID, req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleMemberUpdate changes a member's role. Owners only.
func (s *Server) handleMemberUpdate(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("org")
	if _, ok := s.requireOwner(w, r, orgID); !ok {
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Orgs.SetRole(orgID, r.PathValue("email"), req.Role); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleMemberRemove drops a member. Owners only.
func (s *Server) handleMemberRemove(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("org")
	if _, ok := s.requireOwner(w, r, orgID); !ok {
		return
	}
	if err := s.Orgs.RemoveMember(orgID, r.PathValue("email")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleInviteList shows an org's live invite links. Owners only.
func (s *Server) handleInviteList(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("org")
	if _, ok := s.requireOwner(w, r, orgID); !ok {
		return
	}
	invs := s.Orgs.ListInvites(orgID)
	out := make([]map[string]any, 0, len(invs))
	for _, inv := range invs {
		out = append(out, map[string]any{
			"token": inv.Token, "url": requestBaseURL(r) + "/join/" + inv.Token,
			"creator": inv.Creator, "created": inv.Created, "expires": inv.Expires, "uses": inv.Uses,
		})
	}
	writeJSON(w, map[string]any{"invites": out})
}

// handleInviteRevoke kills an invite link. Owners only.
func (s *Server) handleInviteRevoke(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("org")
	if _, ok := s.requireOwner(w, r, orgID); !ok {
		return
	}
	// Confirm the invite belongs to this org before revoking.
	inv, ok := s.Orgs.Redeem(r.PathValue("token"))
	if !ok || inv.Org != orgID {
		http.Error(w, "no such invite", http.StatusNotFound)
		return
	}
	s.Orgs.RevokeInvite(r.PathValue("token"))
	writeJSON(w, map[string]any{"ok": true})
}

// handleInviteCreate mints an invite link. Owners only.
func (s *Server) handleInviteCreate(w http.ResponseWriter, r *http.Request) {
	if s.Orgs == nil {
		http.Error(w, "organizations are not enabled on this server", http.StatusNotFound)
		return
	}
	orgID := r.PathValue("org")
	if s.Orgs.Role(orgID, s.requestUser(r).Email) != RoleOwner {
		http.Error(w, "only an organization owner can invite", http.StatusForbidden)
		return
	}
	var req struct {
		ExpiresIn string `json:"expires_in,omitempty"` // Go duration, e.g. "168h"
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req) // body is optional
	var ttl time.Duration
	if req.ExpiresIn != "" {
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil || d <= 0 {
			http.Error(w, "invalid expires_in", http.StatusBadRequest)
			return
		}
		ttl = d
	}
	inv, err := s.Orgs.CreateInvite(orgID, s.requestUser(r).Email, ttl)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"token":   inv.Token,
		"url":     requestBaseURL(r) + "/join/" + inv.Token,
		"expires": inv.Expires,
	})
}

// handleInviteAccept joins the signed-in account to the invite's org.
func (s *Server) handleInviteAccept(w http.ResponseWriter, r *http.Request) {
	if s.Orgs == nil {
		http.Error(w, "organizations are not enabled on this server", http.StatusNotFound)
		return
	}
	me := s.requestUser(r)
	if me.Email == "" {
		http.Error(w, "sign in to accept an invite", http.StatusUnauthorized)
		return
	}
	inv, ok := s.Orgs.Redeem(r.PathValue("token"))
	if !ok {
		http.Error(w, "this invite is invalid or expired", http.StatusNotFound)
		return
	}
	org, _ := s.Orgs.Get(inv.Org)
	if org.Members[normEmail(me.Email)] == "" {
		if err := s.quota().CheckSeat(inv.Org, len(org.Members)); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	}
	newMember := org.Members[normEmail(me.Email)] == ""
	if err := s.Orgs.AddMember(inv.Org, me.Email, RoleMember); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if newMember {
		s.Orgs.RecordInviteUse(r.PathValue("token"))
	}
	writeJSON(w, map[string]any{"ok": true, "org": map[string]string{"id": org.ID, "name": org.Name}})
}
