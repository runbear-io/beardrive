package webapp

import (
	"encoding/json"
	"io"
	"net/http"
)

// Administration surfaces: project lifecycle (rename/delete by the owning
// org's owner), hub-admin approval of pending signups, and an org-wide view
// of public share links. All of this is what makes a hub actually
// operable — an admin can offboard, clean up, and audit — without editing
// JSON files on the server by hand.

// projectOwner returns true when the request's account owns the project's org.
func (s *Server) projectOwner(r *http.Request, projectID string) bool {
	if s.Orgs == nil || s.Auth == nil {
		return true
	}
	org := s.orgOf(projectID)
	if org == "" {
		return true
	}
	return s.Orgs.Role(org, s.requestUser(r).Email) == RoleOwner
}

// handleProjectRename renames a project. Owner of its org only.
func (s *Server) handleProjectRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	if s.Projects == nil {
		http.Error(w, "this server does not host projects", http.StatusNotFound)
		return
	}
	if _, ok := s.Projects.Get(id); !ok || !s.projectAllowed(r, id) {
		http.Error(w, "no such project", http.StatusNotFound)
		return
	}
	if !s.projectOwner(r, id) {
		http.Error(w, "only an organization owner can rename a project", http.StatusForbidden)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Projects.Rename(id, req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleProjectDelete removes a project from the registry. Owner only.
// Storage (blobs, journals) is intentionally left in place.
func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	if s.Projects == nil {
		http.Error(w, "this server does not host projects", http.StatusNotFound)
		return
	}
	if _, ok := s.Projects.Get(id); !ok || !s.projectAllowed(r, id) {
		http.Error(w, "no such project", http.StatusNotFound)
		return
	}
	if !s.projectOwner(r, id) {
		http.Error(w, "only an organization owner can delete a project", http.StatusForbidden)
		return
	}
	if err := s.Projects.Delete(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleOrgShares lists every live public share across the org's projects,
// so an owner can audit "what have we made public?" in one place. Any org
// member may view; only owners revoke (via the existing per-share endpoint).
func (s *Server) handleOrgShares(w http.ResponseWriter, r *http.Request) {
	if s.Shares == nil || s.Orgs == nil {
		http.Error(w, "sharing is not enabled on this server", http.StatusNotFound)
		return
	}
	orgID := r.PathValue("org")
	if s.Orgs.Role(orgID, s.requestUser(r).Email) == "" {
		http.Error(w, "you are not a member of this organization", http.StatusForbidden)
		return
	}
	out := []map[string]any{}
	for _, p := range s.Projects.List() {
		if p.Org != orgID {
			continue
		}
		for _, sh := range s.Shares.List(p.ID) {
			j := shareJSON(r, sh)
			j["project_name"] = p.Name
			out = append(out, j)
		}
	}
	writeJSON(w, map[string]any{"shares": out})
}

// builtinAuth returns the concrete OSS auth provider, or nil for a swapped
// provider that doesn't support approval.
func (s *Server) builtinAuth() *BuiltinAuth {
	a, _ := s.Auth.(*BuiltinAuth)
	return a
}

// handleAdminPending lists accounts awaiting approval. Hub admins only.
func (s *Server) handleAdminPending(w http.ResponseWriter, r *http.Request) {
	if !s.requestUser(r).Admin {
		http.Error(w, "hub admins only", http.StatusForbidden)
		return
	}
	a := s.builtinAuth()
	if a == nil {
		writeJSON(w, map[string]any{"pending": []any{}})
		return
	}
	writeJSON(w, map[string]any{"pending": a.PendingUsers()})
}

// handleAdminPolicy reads (GET) or updates (POST) the signup/access policy.
// Domains and the admin list are reported read-only — they're server-config
// owned so a browser session can't widen access — while verification and
// approval toggles can be flipped live and are persisted.
func (s *Server) handleAdminPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requestUser(r).Admin {
		http.Error(w, "hub admins only", http.StatusForbidden)
		return
	}
	a := s.builtinAuth()
	if a == nil {
		http.Error(w, "policy is not supported by this auth provider", http.StatusNotFound)
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			RequireVerification bool `json:"require_verification"`
			RequireApproval     bool `json:"require_approval"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Email verification is only a real gate with a mailer; refuse to turn
		// it on without SMTP rather than silently logging links.
		if req.RequireVerification && a.Mail == nil {
			http.Error(w, "email verification needs SMTP configured on the server", http.StatusBadRequest)
			return
		}
		if err := a.SetPolicy(req.RequireVerification, req.RequireApproval); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	admins := make([]string, 0, len(a.Admins))
	for e := range a.Admins {
		admins = append(admins, e)
	}
	writeJSON(w, map[string]any{
		"require_verification": a.RequireVerification,
		"require_approval":     a.RequireApproval,
		"allow_signup":         a.AllowSignup,
		"allowed_domains":      a.AllowedDomains, // read-only (server config)
		"admins":               admins,           // read-only (server config)
		"mailer":               a.Mail != nil,
	})
}

// handleAdminApprove activates a pending account. Hub admins only.
func (s *Server) handleAdminApprove(w http.ResponseWriter, r *http.Request) {
	if !s.requestUser(r).Admin {
		http.Error(w, "hub admins only", http.StatusForbidden)
		return
	}
	a := s.builtinAuth()
	if a == nil {
		http.Error(w, "approval is not supported by this auth provider", http.StatusNotFound)
		return
	}
	if err := a.Approve(r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleAdminDeny removes a pending account. Hub admins only.
func (s *Server) handleAdminDeny(w http.ResponseWriter, r *http.Request) {
	if !s.requestUser(r).Admin {
		http.Error(w, "hub admins only", http.StatusForbidden)
		return
	}
	a := s.builtinAuth()
	if a == nil {
		http.Error(w, "approval is not supported by this auth provider", http.StatusNotFound)
		return
	}
	if err := a.Deny(r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
