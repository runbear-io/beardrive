package webapp

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
)

// Per-project permissions. Orgs wall projects off from outsiders; these four
// ordered levels say what an insider may do with one project. The default
// level for a project is "write" — today's behavior — expressed as the empty
// string on Project.Default, so an existing hub upgrades with no migration and
// no change in behavior until someone edits permissions.
//
// One resolver (projectPerm, and projectPermOf when the caller already holds
// the Project — same rules, one fewer registry read) and one choke point (the
// proj() wrapper in server.go): every per-project route declares the level it
// needs at registration, so no handler grows its own check and a missed
// handler cannot become a silent authorization hole.

const (
	PermNone  = "none"  // the project is hidden: absent from the list, 403 everywhere
	PermRead  = "read"  // browse, view, download, history, heat
	PermWrite = "write" // + upload, sync push, share links
	PermAdmin = "admin" // + rename, delete, edit this project's permissions
)

// permRank orders the levels. An unknown level ranks as none: fail closed.
func permRank(level string) int {
	switch level {
	case PermRead:
		return 1
	case PermWrite:
		return 2
	case PermAdmin:
		return 3
	default:
		return 0
	}
}

// atLeast reports whether have satisfies want.
func atLeast(have, want string) bool { return permRank(have) >= permRank(want) }

// validLevel says whether a level is one an API caller may name.
func validLevel(l string) bool {
	return l == PermNone || l == PermRead || l == PermWrite || l == PermAdmin
}

// projectPerm resolves the request account's effective level on a project:
//
//	org owner of the project's org  → admin (always; never lockable-out)
//	not a member of that org        → none (whatever grants the project holds)
//	explicit grant                  → that level ("none" = denied)
//	member of the project's org      → the project default (write unless changed)
//	otherwise                       → none
//
// Org membership is resolved before any grant is consulted: RemoveMember does
// not walk every project's grant map, so a grant written while an account was
// a member would otherwise outlive the membership and offboarding through the
// API would not offboard. grantable() already refuses to create a grant for a
// non-member; this is the same rule at read time.
//
// The one escape hatch left is single-volume mode (no directory, no auth),
// where there is no membership model to consult. An unknown project id and an
// org-less project both fail closed: no API path produces either on a
// configured hub, and "inherited from code that no longer exists" is not a
// reason to keep an escape that makes a project world-writable.
func (s *Server) projectPerm(r *http.Request, projectID string) string {
	if s.Desktop {
		return PermRead // the desktop viewer is read-only, for everyone
	}
	if s.Dir == nil || s.Auth == nil {
		return capByGrant(r, projectID, PermAdmin)
	}
	p, ok := s.Projects.Get(projectID)
	if !ok {
		return PermNone
	}
	return s.projectPermOf(r, p)
}

// projectPermOf is projectPerm for a caller that has already resolved the
// project — every per-project route does, in proj(). Resolving it twice per
// request re-read the whole registry twice (see ProjectDB.refresh): 24 ms a
// request at 5k projects on the file backend, nine unfiltered SELECTs on
// Postgres. Same rules, same fail-closed defaults; the resolution just does
// not happen again.
func (s *Server) projectPermOf(r *http.Request, p Project) string {
	// The grant ceiling is applied OUTSIDE the resolution, not on one branch
	// of it. Every early return below is a path that answers without asking
	// who the caller is — Desktop, and a hub with no directory — and an MCP
	// token reaching one of those would have come back PermAdmin on a project
	// its grant never named. A ceiling that only covers the common branch is
	// the same shape of bug as a gate wired to one concrete type: enforced
	// further than it can be satisfied, and silent about the gap.
	return capByGrant(r, p.ID, s.resolveProjectPerm(r, p))
}

func (s *Server) resolveProjectPerm(r *http.Request, p Project) string {
	if s.Desktop {
		return PermRead // the desktop viewer is read-only, for everyone
	}
	if s.Dir == nil || s.Auth == nil {
		return PermAdmin
	}
	if p.Org == "" {
		return PermNone
	}
	return s.projectPermFor(p, normEmail(s.requestUser(r).Email))
}

// projectPermFor is projectPermOf's core, keyed on an account rather than a
// request. Split out for the one caller that has no request user to speak of:
// a public share link, which has to ask whether the account that MINTED it can
// still read what it publishes (shareStillReadable).
func (s *Server) projectPermFor(p Project, email string) string {
	role := s.roleIn(p.Org, email)
	if role == RoleApp {
		p = s.honoredGrants(p, email)
	}
	return projectPermWithRole(p, email, role)
}

// mayGrant says whether a member could give a grant of level on a folder
// (prefix "" for the whole project) as the project stands: read is any
// writer's to give, more than read only an admin's.
func (s *Server) mayGrant(p Project, by, prefix, level string) bool {
	return atLeast(s.grantCeiling(p, by, prefix), level)
}

// grantCeiling is the most a member could give on a folder (prefix "" for the
// project) as the project stands: anything for an admin, read for a writer,
// nothing for anyone else.
func (s *Server) grantCeiling(p Project, by, prefix string) string {
	if by == "" || isAppPrincipal(by) {
		return PermNone
	}
	at := s.projectPermFor(p, by)
	if prefix != "" {
		at = effectiveLevel(p, by, prefix, at)
	}
	switch at {
	case PermAdmin:
		return PermAdmin
	case PermWrite:
		return PermRead
	}
	return PermNone
}

// lowerOf is the lower of two levels.
func lowerOf(a, b string) string {
	if atLeast(a, b) {
		return b
	}
	return a
}

// honoredGrants is p as one app may use it. Each grant the app holds counts
// only as far as the member who gave it could still give it, judged on every
// request: an app set up by a member who has since left, been demoted or been
// shut out of a folder stops reading there at once, with nothing to clean up.
// A grant with no recorded giver predates attribution and was an admin's or an
// owner's; it counts as it always has.
//
// Two kinds of grant reach a path. Where the longest folder rule names the
// app, that grant decides, held to its giver on that folder. Elsewhere the
// app's project grant decides, through the rule's default, so the project
// grant's giver is held to what they could give on that folder too. Rules are
// the only places a member's level changes within a project, so holding each
// one covers every path. A cap only ever lowers what the app would have had.
//
// An app with folder grants and no project grant reads those folders and
// nothing else. It is given read on the project only so the folders are
// reachable at all, and every path none of its own grants reaches is denied:
// a rule on "" covers paths no rule matches, and a denial covers each rule
// that does not name the app.
func (s *Server) honoredGrants(p Project, app string) Project {
	ceilings := map[[2]string]string{}
	honor := func(level, by, prefix string) string {
		if by == "" || level == PermNone {
			return level
		}
		k := [2]string{by, prefix}
		c, ok := ceilings[k]
		if !ok {
			c = s.grantCeiling(p, by, prefix)
			ceilings[k] = c
		}
		return lowerOf(level, c)
	}
	v := p.clone()
	level, granted := v.Perms[app]
	by := v.GrantedBy[app]
	if granted {
		if level = honor(level, by, ""); level == PermNone {
			delete(v.Perms, app)
			granted = false
		} else {
			v.Perms[app] = level
		}
	}
	reaches := false // the app reads some folder by a grant of its own
	for i := range v.Folders {
		rule := &v.Folders[i]
		if l, ok := rule.Perms[app]; ok {
			if h := honor(l, rule.GrantedBy[app], rule.Prefix); h != PermNone || l == PermNone {
				rule.Perms[app] = h
				reaches = reaches || atLeast(h, PermRead)
				continue
			}
			// Its giver can give nothing here now: the folder falls to the
			// rule's default, like a folder the app was never named on.
			delete(rule.Perms, app)
		}
		if !granted { // only the app's own folder grants open anything
			if rule.Perms == nil {
				rule.Perms = map[string]string{}
			}
			rule.Perms[app] = PermNone
			continue
		}
		would := rule.Default
		if would == "" {
			would = level
		}
		if capped := honor(would, by, rule.Prefix); capped != would {
			if rule.Perms == nil {
				rule.Perms = map[string]string{}
			}
			rule.Perms[app] = capped
		}
	}
	if !granted && reaches {
		if v.Perms == nil {
			v.Perms = map[string]string{}
		}
		v.Perms[app] = PermRead
		v.Folders = append(v.Folders, FolderRule{Prefix: "", Perms: map[string]string{app: PermNone}})
	}
	return v
}

// projectPermWithRole is projectPermFor once the account's role in the
// project's org is known.
func projectPermWithRole(p Project, email, role string) string {
	if role == RoleOwner {
		return PermAdmin
	}
	if role == "" {
		return PermNone // not a member of the project's org
	}
	// An app holds what it was granted and nothing else: no project default,
	// and never admin, which would let a server key manage people.
	if role == RoleApp {
		switch l := p.Perms[email]; l {
		case "":
			return PermNone
		case PermAdmin:
			return PermWrite
		default:
			return l
		}
	}
	if l, ok := p.Perms[email]; ok {
		return l
	}
	return p.level()
}

// requirePerm answers the request itself when the caller is short of level.
func (s *Server) requirePerm(w http.ResponseWriter, r *http.Request, projectID, level string) bool {
	return s.permit(w, s.projectPerm(r, projectID), level)
}

// requirePermOn is requirePerm for a caller holding the resolved project.
func (s *Server) requirePermOn(w http.ResponseWriter, r *http.Request, p Project, level string) bool {
	return s.permit(w, s.projectPermOf(r, p), level)
}

func (s *Server) permit(w http.ResponseWriter, have, want string) bool {
	if atLeast(have, want) {
		return true
	}
	http.Error(w, permDenied(want), http.StatusForbidden)
	return false
}

// permDenied is the operator-voice 403 body. Deliberately one shape for every
// level so the frontend's errorFor keeps mapping it.
func permDenied(level string) string {
	switch level {
	case PermAdmin:
		return "you need admin permission on this project"
	case PermWrite:
		return "you have read-only access to this project"
	default:
		return "you do not have access to this project"
	}
}

// ---- HTTP ----

// handleProjectPerms returns the project's permission settings: the default
// level, the caller's own effective level, and the explicit grants. Any member
// with read may look; the grants are org-internal, not secrets.
//
// Re-confirmed by the owner in BEA-69 after a read-only member reported seeing
// the whole People matrix: the benchmark is Google Drive, where a viewer can
// see who has access, and hiding grants makes "why can't I edit this?"
// unanswerable. Same call for the project's public-links list
// (handleShareList, also PermRead). Raising either to PermWrite is a product
// decision, not a hardening fix — TestReadMemberSeesSharesAndGrants pins both.
func (s *Server) handleProjectPerms(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	p, ok := s.project(w, r, id, PermRead)
	if !ok {
		return
	}
	grants := make([]map[string]string, 0, len(p.Perms))
	for email, level := range p.Perms {
		grants = append(grants, map[string]string{"email": email, "level": level})
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i]["email"] < grants[j]["email"] })
	writeJSON(w, map[string]any{
		"default": p.level(),
		"me":      s.projectPermOf(r, p),
		"creator": p.Creator,
		"grants":  grants,
	})
}

// handleProjectPermDefault sets the level every org member gets without an
// explicit grant. Admin only. "admin" is not a legal default: it would make
// the last-admin rule meaningless.
func (s *Server) handleProjectPermDefault(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	if _, ok := s.project(w, r, id, PermAdmin); !ok {
		return
	}
	var req struct {
		Default string `json:"default"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !validLevel(req.Default) || req.Default == PermAdmin {
		http.Error(w, "default must be none, read, or write", http.StatusBadRequest)
		return
	}
	if err := s.Projects.SetDefault(id, req.Default); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The project level is the base every folder rule narrows or widens, so
	// moving it moves every reader's scope — and scopeTag with it.
	s.publishScope(id)
	writeJSON(w, map[string]any{"ok": true})
}

// handleProjectPermSet grants one account an explicit level. Admin only.
//
// A project admin may give anyone any level. A member who can write the
// project may give an app read, on the project or on a folder they can write,
// and nothing else: the key that reads through it is a safer credential than
// the device token they can already mint. Either way an app's grant records
// who gave it, and counts only while they could still give it (honoredGrants).
// "path" names a folder; without it the grant is on the project.
func (s *Server) handleProjectPermSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	p, ok := s.project(w, r, id, PermWrite)
	if !ok {
		return
	}
	var req struct {
		Level string `json:"level"`
		Path  string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !validLevel(req.Level) {
		http.Error(w, "level must be none, read, write, or admin", http.StatusBadRequest)
		return
	}
	email := normEmail(r.PathValue("email"))
	prefix, ok := s.grantPrefix(w, req.Path)
	if !ok {
		return
	}
	me, admin := s.granter(r, p)
	if !admin && !s.writerMayGrant(w, p, me, email, prefix, req.Level) {
		return
	}
	if !s.grantable(w, p, email) {
		return
	}
	if isAppPrincipal(email) && req.Level == PermAdmin {
		http.Error(w, "an app can be granted read or write, never admin", http.StatusBadRequest)
		return
	}
	if prefix != "" && req.Level == PermAdmin {
		http.Error(w, "a folder grant must be none, read, or write", http.StatusBadRequest)
		return
	}
	by := ""
	if isAppPrincipal(email) {
		by = me
	}
	var err error
	if prefix == "" {
		err = s.Projects.SetPermBy(id, email, req.Level, by)
	} else {
		err = s.Projects.SetFolderGrant(id, prefix, email, req.Level, by)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.publishScope(id)
	writeJSON(w, map[string]any{"ok": true})
}

// handleProjectPermClear drops an explicit grant, reverting that account to
// the project default (or, with ?path=, that folder's grant to the rule). A
// project admin may drop any; a member who can write may drop an app grant
// they gave.
func (s *Server) handleProjectPermClear(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	p, ok := s.project(w, r, id, PermWrite)
	if !ok {
		return
	}
	email := normEmail(r.PathValue("email"))
	prefix, ok := s.grantPrefix(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	if me, admin := s.granter(r, p); !admin && !s.ownGrant(w, p, me, email, prefix) {
		return
	}
	var err error
	if prefix == "" {
		err = s.Projects.ClearPerm(id, email)
	} else {
		err = s.Projects.DropFolderGrant(id, prefix, email)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.publishScope(id)
	writeJSON(w, map[string]any{"ok": true})
}

// grantPrefix normalizes the folder a grant names, "" for the whole project.
func (s *Server) grantPrefix(w http.ResponseWriter, path string) (string, bool) {
	if strings.Trim(strings.TrimSpace(path), "/") == "" {
		return "", true
	}
	prefix, err := normPrefix(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "", false
	}
	return prefix, true
}

// granter is who is giving a grant and whether they administer the project.
func (s *Server) granter(r *http.Request, p Project) (string, bool) {
	return normEmail(s.requestUser(r).Email), atLeast(s.projectPermOf(r, p), PermAdmin)
}

// writerMayGrant answers unless a member who is not a project admin may give
// this grant: read, to an app, where they can write, and not over a grant
// someone else gave.
func (s *Server) writerMayGrant(w http.ResponseWriter, p Project, me, email, prefix, level string) bool {
	if !isAppPrincipal(email) || level != PermRead {
		http.Error(w, "only a project admin can give that; a member who can write may give an app read",
			http.StatusForbidden)
		return false
	}
	if !s.mayGrant(p, me, prefix, level) {
		http.Error(w, "you can give an app read only where you can write", http.StatusForbidden)
		return false
	}
	return s.ownGrant(w, p, me, email, prefix)
}

// ownGrant answers unless the app's grant there, if it has one, was given by
// me. A member who is not an admin must not replace or remove a grant an admin
// or another member gave.
func (s *Server) ownGrant(w http.ResponseWriter, p Project, me, email, prefix string) bool {
	if !isAppPrincipal(email) {
		http.Error(w, "only a project admin can change that", http.StatusForbidden)
		return false
	}
	_, has := p.Perms[email]
	by := p.GrantedBy[email]
	if prefix != "" {
		has, by = false, ""
		for _, rule := range p.Folders {
			if rule.Prefix == prefix {
				_, has = rule.Perms[email]
				by = rule.GrantedBy[email]
			}
		}
	}
	if has && by != me {
		http.Error(w, "someone else gave this app that grant; ask a project admin to change it", http.StatusForbidden)
		return false
	}
	return true
}

// grantable checks the target of a grant: org members only, and never an org
// owner (they are implicitly admin everywhere, so a grant on one would be
// silently ignored — and a write that quietly does nothing is worse than a
// refusal).
func (s *Server) grantable(w http.ResponseWriter, p Project, email string) bool {
	if s.Dir == nil || p.Org == "" {
		return true
	}
	switch s.roleIn(p.Org, email) {
	case "":
		http.Error(w, "that account is not a member of this project's organization", http.StatusBadRequest)
		return false
	case RoleOwner:
		http.Error(w, "organization owners are always project admins", http.StatusBadRequest)
		return false
	}
	return true
}

// project resolves a project id and the caller's level in one step, answering
// the request itself when either fails. A missing project is 404; an existing
// one the caller may not touch is 403 — the same answer a non-member gets, so
// the two are indistinguishable from outside.
func (s *Server) project(w http.ResponseWriter, r *http.Request, id, level string) (Project, bool) {
	if s.Projects == nil {
		http.Error(w, "this server does not host projects", http.StatusNotFound)
		return Project{}, false
	}
	p, ok := s.Projects.Get(id)
	if !ok {
		http.Error(w, "no such project", http.StatusNotFound)
		return Project{}, false
	}
	if !s.requirePermOn(w, r, p, level) {
		return Project{}, false
	}
	return p, true
}

// membershipLister is a Directory that can answer every org an account is in
// from one read. Optional: without it orgRoles asks Role once per org.
type membershipLister interface {
	Memberships(email string) map[string]string
}

// orgRoles answers "what is this account's role in org X" for many orgs
// within one request, reading the directory once rather than once per
// project. Every answer comes from the same read, so a request sees one
// consistent membership — it is per request, never kept across requests.
func (s *Server) orgRoles(email string) func(org string) string {
	if ml, ok := s.Dir.(membershipLister); ok {
		roles := ml.Memberships(email)
		return func(org string) string { return roles[org] }
	}
	memo := map[string]string{}
	return func(org string) string {
		r, ok := memo[org]
		if !ok {
			r = s.Dir.Role(org, email)
			memo[org] = r
		}
		return r
	}
}
