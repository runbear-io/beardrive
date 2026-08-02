package webapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
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
		db.repo.PutInvite(inv)
	}
}

func (i OrgInvite) expired() bool { return time.Now().After(i.Expires) }

// DefaultInviteTTL bounds invite links that don't ask for an expiry.
const DefaultInviteTTL = 7 * 24 * time.Hour

// OrgDB is the in-memory org registry over a MetaStore OrgRepo (orgs + invites).
type OrgDB struct {
	repo OrgRepo

	mu      sync.Mutex
	byID    map[string]Org
	invites map[string]OrgInvite
}

// NewOrgDB builds the registry over a repo, loading orgs and invites.
func NewOrgDB(repo OrgRepo) (*OrgDB, error) {
	db := &OrgDB{repo: repo, byID: make(map[string]Org), invites: make(map[string]OrgInvite)}
	orgs, invites, err := repo.Load()
	if err != nil {
		return nil, err
	}
	for _, o := range orgs {
		db.byID[o.ID] = o
	}
	for _, i := range invites {
		db.invites[i.Token] = i
	}
	return db, nil
}

// OpenOrgDB loads the file-backed registry at path.
func OpenOrgDB(path string) (*OrgDB, error) {
	return NewOrgDB(newFileOrgRepo(path))
}

func normEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// clone copies an Org so its Members map never leaves the registry: Org is a
// value but the map inside it is a pointer, and a caller holding the live map
// can write roles straight past SetRole, the last-owner guard and the store —
// or race a concurrent mutator while handleOrgList ranges over it.
func (o Org) clone() Org {
	c := o
	c.Members = make(map[string]string, len(o.Members))
	for k, v := range o.Members {
		c.Members[k] = v
	}
	return c
}

// putOrg persists a mutated org, restoring the previous value if the store
// refuses. Callers hold mu. Without the rollback a refused write still reads
// as applied until the hub restarts, when it silently reverts — a demotion
// that un-demotes itself.
func (db *OrgDB) putOrg(prev, o Org) error {
	db.byID[o.ID] = o
	if err := db.repo.PutOrg(o); err != nil {
		db.byID[o.ID] = prev
		return err
	}
	return nil
}

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
	if err := db.repo.PutOrg(o); err != nil {
		delete(db.byID, o.ID)
		return Org{}, err
	}
	return o.clone(), nil
}

func (db *OrgDB) Get(id string) (Org, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	o, ok := db.byID[id]
	if !ok {
		return Org{}, false
	}
	return o.clone(), true
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
			out = append(out, o.clone())
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
	next := o.clone()
	next.Members[e] = role
	return db.putOrg(o, next)
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
	if o.Members[e] == RoleOwner && db.ownerCount(o) <= 1 {
		return fmt.Errorf("cannot remove the last owner")
	}
	return db.removeLocked(o, e)
}

// EvictMember drops an account from the org unconditionally — the form
// offboard needs when the ACCOUNT itself is gone. The last-owner rule keeps a
// LIVE org administrable; it must never preserve an ownership row for an
// address nobody can sign in as, because the next signup on that address
// inherits it — org ownership, and through it admin on every project in the
// org. An org left with no owner is a recovery problem, not an authorization
// one.
func (db *OrgDB) EvictMember(orgID, email string) error {
	e := normEmail(email)
	db.mu.Lock()
	defer db.mu.Unlock()
	o, ok := db.byID[orgID]
	if !ok {
		return fmt.Errorf("no such organization")
	}
	return db.removeLocked(o, e)
}

// removeLocked deletes a member row. Callers hold mu.
func (db *OrgDB) removeLocked(o Org, e string) error {
	if o.Members[e] == "" {
		return nil // already not a member: the postcondition already holds
	}
	next := o.clone()
	delete(next.Members, e)
	return db.putOrg(o, next)
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
	next := o.clone()
	next.Members[e] = role
	return db.putOrg(o, next)
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
	next := o
	next.Name = name
	return db.putOrg(o, next)
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
	inv, ok := db.invites[token]
	if !ok {
		return false
	}
	delete(db.invites, token)
	if err := db.repo.DeleteInvite(token); err != nil {
		// Revocation is the emergency stop for a leaked join link, and on an
		// invite-only hub that link bootstraps accounts. A delete the store
		// refused would come back at the next restart, so put it back and
		// report the failure instead of reporting a revocation that isn't one.
		db.invites[token] = inv
		return false
	}
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
	if err := db.repo.PutInvite(inv); err != nil {
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
// orgWriter is the slice of Directory that MigrateOrgs needs: it creates one
// org and fills it. Taking the narrow type keeps the sweep usable with a bare
// OrgDB (which is what the CLI has at that point) instead of forcing a
// LocalDirectory wrapper on a function that has no use for ManageURL.
type orgWriter interface {
	Create(name, ownerEmail string) (Org, error)
	AddMember(orgID, email, role string) error
}

func MigrateOrgs(projects *ProjectDB, orgs orgWriter, accounts []User) error {
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

// offboard drops every grant an address holds, at the moment its ACCOUNT goes
// away. Every authorization decision on the hub keys on the email — OrgDB.Role,
// Project.Perms, and share liveness through shareCreatorStillBelongs — and
// account removal touched none of them, so the grants outlived the account and
// the next account on that address (a re-signup, a redeemed invite, an admin
// re-adding someone) inherited them, project admin included. Round 1 ruled a
// grant must not outlive org membership; this is the same rule one level up.
//
// One choke point rather than N sweeps: it is wired into the hub's only
// account-removal path (BuiltinAuth.Deny) in Handler.
func (s *Server) offboard(email string) {
	e := normEmail(email)
	if e == "" {
		return
	}
	if s.Projects != nil {
		for _, p := range s.Projects.List() {
			if _, has := p.Perms[e]; has {
				if err := s.Projects.dropPerm(p.ID, e); err != nil {
					log.Printf("beardrive: offboard %s: project %s: %v", e, p.ID, err)
				}
			}
		}
	}
	if s.Dir != nil {
		// Last, because membership is what share liveness resolves through:
		// clearing it is what makes a removed account's public links stop
		// serving.
		//
		// "The account is gone" is authoritative here: RemoveMember refuses to
		// drop the last owner, and logging that refusal left the hub's most
		// privileged row attached to an address anyone could then sign up on.
		// Evict instead, where the directory owns its orgs.
		drop := s.Dir.RemoveMember
		if ev, ok := s.Dir.(orgEvictor); ok {
			drop = ev.EvictMember
		}
		for _, o := range s.Dir.OrgsFor(e) {
			if err := drop(o.ID, e); err != nil {
				log.Printf("beardrive: offboard %s: org %s NOT removed, the address keeps its grants: %v",
					e, o.ID, err)
			}
		}
	}
}

// orgEvictor is the part of a directory that can drop a row for an account
// that no longer exists. LocalDirectory (OrgDB) implements it; a directory
// managing its orgs elsewhere does not, and offboard reports the failure
// rather than hiding it.
type orgEvictor interface {
	EvictMember(orgID, email string) error
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

// handleOrgList returns the caller's orgs with members (visible to any
// member) and the caller's role.
func (s *Server) handleOrgList(w http.ResponseWriter, r *http.Request) {
	if s.Dir == nil {
		writeJSON(w, map[string]any{"orgs": []any{}})
		return
	}
	me := s.requestUser(r)
	out := []map[string]any{}
	for _, o := range s.Dir.OrgsFor(me.Email) {
		members := make([]map[string]string, 0, len(o.Members))
		for email, role := range o.Members {
			members = append(members, map[string]string{"email": email, "role": role})
		}
		sort.Slice(members, func(i, j int) bool { return members[i]["email"] < members[j]["email"] })
		out = append(out, map[string]any{
			"id": o.ID, "name": o.Name, "role": o.Members[normEmail(me.Email)],
			"members": members, "created": o.Created,
			"manage_url": s.Dir.ManageURL(o.ID),
		})
	}
	writeJSON(w, map[string]any{"orgs": out})
}

// writeDirErr answers a failed directory write. A directory that does not own
// its organizations says so with ErrManagedElsewhere, and the answer is 409
// plus the page that does own them — the request was well-formed, it is the
// state of the world that makes it wrong. The hub never learns WHY the write
// was refused, only where to send the user.
func (s *Server) writeDirErr(w http.ResponseWriter, orgID string, err error) {
	if errors.Is(err, ErrManagedElsewhere) {
		http.Error(w, err.Error()+": "+s.Dir.ManageURL(orgID), http.StatusConflict)
		return
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
}

// requireOwner returns true and the caller's email when they own the org;
// otherwise it writes the error response and returns false.
func (s *Server) requireOwner(w http.ResponseWriter, r *http.Request, orgID string) (string, bool) {
	if s.Dir == nil {
		http.Error(w, "organizations are not enabled on this server", http.StatusNotFound)
		return "", false
	}
	me := s.requestUser(r)
	if s.Dir.Role(orgID, me.Email) != RoleOwner {
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
	if err := s.Dir.Rename(orgID, req.Name); err != nil {
		s.writeDirErr(w, orgID, err)
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
	if err := s.Dir.SetRole(orgID, r.PathValue("email"), req.Role); err != nil {
		s.writeDirErr(w, orgID, err)
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
	if err := s.Dir.RemoveMember(orgID, r.PathValue("email")); err != nil {
		s.writeDirErr(w, orgID, err)
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
	invs := s.Dir.ListInvites(orgID)
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
	inv, ok := s.Dir.Redeem(r.PathValue("token"))
	if !ok || inv.Org != orgID {
		http.Error(w, "no such invite", http.StatusNotFound)
		return
	}
	s.Dir.RevokeInvite(r.PathValue("token"))
	writeJSON(w, map[string]any{"ok": true})
}

// handleInviteCreate mints an invite link. Owners only.
func (s *Server) handleInviteCreate(w http.ResponseWriter, r *http.Request) {
	if s.Dir == nil {
		http.Error(w, "organizations are not enabled on this server", http.StatusNotFound)
		return
	}
	orgID := r.PathValue("org")
	if s.Dir.Role(orgID, s.requestUser(r).Email) != RoleOwner {
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
	inv, err := s.Dir.CreateInvite(orgID, s.requestUser(r).Email, ttl)
	if err != nil {
		s.writeDirErr(w, orgID, err)
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
	if s.Dir == nil {
		http.Error(w, "organizations are not enabled on this server", http.StatusNotFound)
		return
	}
	// Normalized, because every decision downstream is: AddMember, Role and
	// the grant maps all key on normEmail. Guarding on the raw string left the
	// values in between — "   ", "\t" — running Redeem and the seat check
	// before AddMember refused, which is an invite-token validity oracle for a
	// principal the hub cannot name (an invite bootstraps an account on the
	// default, invite-only posture).
	me := s.requestUser(r)
	if normEmail(me.Email) == "" {
		http.Error(w, "sign in to accept an invite", http.StatusUnauthorized)
		return
	}
	// Check-and-add is one operation: the seat check counts members and the
	// join adds one, so without this the last seat can be sold twice.
	s.joinMu.Lock()
	defer s.joinMu.Unlock()
	inv, ok := s.Dir.Redeem(r.PathValue("token"))
	if !ok {
		http.Error(w, "this invite is invalid or expired", http.StatusNotFound)
		return
	}
	org, _ := s.Dir.Get(inv.Org)
	if org.Members[normEmail(me.Email)] == "" {
		if err := s.quota().CheckSeat(inv.Org, len(org.Members)); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	}
	newMember := org.Members[normEmail(me.Email)] == ""
	if err := s.Dir.AddMember(inv.Org, me.Email, RoleMember); err != nil {
		s.writeDirErr(w, inv.Org, err)
		return
	}
	if newMember {
		s.Dir.RecordInviteUse(r.PathValue("token"))
	}
	writeJSON(w, map[string]any{"ok": true, "org": map[string]string{"id": org.ID, "name": org.Name}})
}
