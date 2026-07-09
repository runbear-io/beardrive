package webapp

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Share links make one file publicly readable at /s/<unguessable-token> —
// no sign-in needed, which is the whole point: "here's the report" is just a
// URL. A link always serves the file's LATEST synced content (living wiki
// pages, evolving reports) and lives until revoked, unless created with an
// expiry. Everything else on the hub stays behind auth.
//
// Shared content renders (HTML as a page, markdown Obsidian-style, PDFs
// inline) but sandboxed: /s/ responses carry a strict CSP sandbox and never
// see auth cookies, so a malicious shared file's scripts run in an opaque
// origin and can't touch hub sessions.

// Share is one public link.
type Share struct {
	Token   string    `json:"token"`
	Project string    `json:"project"`
	Path    string    `json:"path"`
	Creator string    `json:"creator,omitempty"` // account email
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires,omitzero"` // zero = permanent until revoked
}

func (s Share) expired() bool {
	return !s.Expires.IsZero() && time.Now().After(s.Expires)
}

// ShareDB is the file-backed share registry (shares.json), same discipline
// as the project registry.
type ShareDB struct {
	path string

	mu      sync.Mutex
	byToken map[string]Share
}

func OpenShareDB(path string) (*ShareDB, error) {
	db := &ShareDB{path: path, byToken: make(map[string]Share)}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return db, nil
		}
		return nil, err
	}
	var file struct {
		Shares []Share `json:"shares"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, s := range file.Shares {
		db.byToken[s.Token] = s
	}
	return db, nil
}

// save persists the registry. Callers hold mu.
func (db *ShareDB) save() error {
	var file struct {
		Shares []Share `json:"shares"`
	}
	for _, s := range db.byToken {
		file.Shares = append(file.Shares, s)
	}
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

// Create returns a share for (project, path), reusing an existing live one
// so repeated shares of the same file hand out the same URL.
func (db *ShareDB) Create(project, p, creator string, ttl time.Duration) (Share, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, s := range db.byToken {
		if s.Project == project && s.Path == p && !s.expired() && s.Expires.IsZero() && ttl == 0 {
			return s, nil
		}
	}
	s := Share{
		Token: randHex(16), Project: project, Path: p,
		Creator: creator, Created: time.Now().UTC(),
	}
	if ttl > 0 {
		s.Expires = time.Now().UTC().Add(ttl)
	}
	db.byToken[s.Token] = s
	if err := db.save(); err != nil {
		delete(db.byToken, s.Token)
		return Share{}, err
	}
	return s, nil
}

// Get resolves a live (non-expired) share.
func (db *ShareDB) Get(token string) (Share, bool) {
	if db == nil {
		return Share{}, false
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	s, ok := db.byToken[token]
	if !ok || s.expired() {
		return Share{}, false
	}
	return s, true
}

func (db *ShareDB) Revoke(token string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.byToken[token]; !ok {
		return false
	}
	delete(db.byToken, token)
	db.save()
	return true
}

// List returns a project's live shares.
func (db *ShareDB) List(project string) []Share {
	db.mu.Lock()
	defer db.mu.Unlock()
	var out []Share
	for _, s := range db.byToken {
		if s.Project == project && !s.expired() {
			out = append(out, s)
		}
	}
	return out
}

// ---- HTTP ----

// handleShareCreate mints (or returns) the share link for a file. Any
// signed-in member can share; the file must already be synced.
func (s *Server) handleShareCreate(v *volume, w http.ResponseWriter, r *http.Request) {
	if s.Shares == nil {
		http.Error(w, "sharing is not enabled on this server", http.StatusNotFound)
		return
	}
	var req struct {
		Path      string `json:"path"`
		ExpiresIn string `json:"expires_in,omitempty"` // Go duration, e.g. "168h"
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	p, err := cleanUploadPath(req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	snap, err := v.snapshot(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if _, ok := snap.files[p]; !ok {
		http.Error(w, fmt.Sprintf("%s is not synced to this project yet", p), http.StatusNotFound)
		return
	}
	var ttl time.Duration
	if req.ExpiresIn != "" {
		if ttl, err = time.ParseDuration(req.ExpiresIn); err != nil || ttl <= 0 {
			http.Error(w, "invalid expires_in", http.StatusBadRequest)
			return
		}
	}
	sh, err := s.Shares.Create(r.PathValue("project"), p, s.requestUser(r).Email, ttl)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, shareJSON(r, sh))
}

func (s *Server) handleShareList(v *volume, w http.ResponseWriter, r *http.Request) {
	if s.Shares == nil {
		http.Error(w, "sharing is not enabled on this server", http.StatusNotFound)
		return
	}
	shares := s.Shares.List(r.PathValue("project"))
	out := make([]map[string]any, 0, len(shares))
	for _, sh := range shares {
		out = append(out, shareJSON(r, sh))
	}
	writeJSON(w, map[string]any{"shares": out})
}

func (s *Server) handleShareRevoke(w http.ResponseWriter, r *http.Request) {
	if s.Shares == nil {
		http.Error(w, "sharing is not enabled on this server", http.StatusNotFound)
		return
	}
	sh, ok := s.Shares.Get(r.PathValue("token"))
	if ok && !s.projectAllowed(r, sh.Project) {
		http.Error(w, "you are not a member of this project's organization", http.StatusForbidden)
		return
	}
	if s.Shares.Revoke(r.PathValue("token")) {
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	http.Error(w, "no such share", http.StatusNotFound)
}

func shareJSON(r *http.Request, sh Share) map[string]any {
	out := map[string]any{
		"token": sh.Token, "path": sh.Path, "project": sh.Project,
		"url": requestBaseURL(r) + "/s/" + sh.Token, "created": sh.Created,
	}
	if sh.Creator != "" {
		out["creator"] = sh.Creator
	}
	if !sh.Expires.IsZero() {
		out["expires"] = sh.Expires
	}
	return out
}

// handleShared serves a share link: public, sandboxed, always the latest
// synced content.
func (s *Server) handleShared(w http.ResponseWriter, r *http.Request) {
	if !s.shareLimiter().allow(clientIP(r)) {
		http.Error(w, "too many requests — slow down", http.StatusTooManyRequests)
		return
	}
	sh, ok := s.Shares.Get(r.PathValue("token"))
	if !ok {
		http.Error(w, "this link does not exist or was revoked", http.StatusNotFound)
		return
	}
	v, err := s.projectVolume(sh.Project)
	if err != nil {
		http.Error(w, "this link does not exist or was revoked", http.StatusNotFound)
		return
	}
	snap, err := v.snapshot(r.Context())
	if err != nil {
		http.Error(w, "content temporarily unavailable", http.StatusBadGateway)
		return
	}
	fi, ok := snap.files[sh.Path]
	if !ok {
		http.Error(w, "the shared file no longer exists", http.StatusNotFound)
		return
	}

	// Sandbox everything under /s/: shared content executes in an opaque
	// origin (scripts allowed — charts in reports — but no cookies, no
	// same-origin reach back into the hub).
	w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-popups")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")

	rc, err := v.source.Open(r.Context(), sh.Path, fi)
	if err != nil {
		http.Error(w, "content temporarily unavailable", http.StatusBadGateway)
		return
	}
	defer rc.Close()

	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Type", contentType(sh.Path))
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", sanitizeFilename(path.Base(sh.Path))))
		io.Copy(w, rc)
		return
	}

	switch strings.ToLower(path.Ext(sh.Path)) {
	case ".md", ".markdown":
		src, err := io.ReadAll(rc)
		if err != nil {
			http.Error(w, "content temporarily unavailable", http.StatusBadGateway)
			return
		}
		body, err := RenderMarkdown(src)
		if err != nil {
			http.Error(w, "render failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, sharedMarkdownShell, html.EscapeString(path.Base(sh.Path)), body)
	case ".html", ".htm":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.Copy(w, rc)
	default:
		w.Header().Set("Content-Type", contentType(sh.Path))
		w.Header().Set("Content-Length", fmt.Sprint(fi.Size))
		io.Copy(w, rc)
	}
}

// sharedMarkdownShell wraps rendered markdown in a minimal readable page.
const sharedMarkdownShell = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>%s</title>
<style>
body{font:16px/1.7 -apple-system,BlinkMacSystemFont,"SF Pro Text","Inter","Segoe UI",sans-serif;color:#24292f;
max-width:720px;margin:0 auto;padding:52px 24px 96px}
a{color:#b26a00}
@media (prefers-color-scheme: dark){body{background:#0a0b0d;color:#c6cbd3}
a{color:#ffcf85}code,pre{background:#15171b}h1,h2,h3{color:#f4f6f9}}
h1,h2,h3{line-height:1.25;letter-spacing:-.018em}
pre{padding:12px;border-radius:8px;overflow-x:auto;background:#f6f8fa}
code{background:#f6f8fa;padding:2px 5px;border-radius:4px;font-size:.9em}
pre code{padding:0;background:none}
img{max-width:100%%}
blockquote{margin:0;padding-left:16px;border-left:3px solid #d0d7de;color:#57606a}
table{border-collapse:collapse;display:block;overflow-x:auto;max-width:100%%}td,th{border:1px solid #d0d7de;padding:5px 10px}
pre{max-width:100%%}
footer.bdrive{margin-top:64px;padding-top:14px;border-top:1px solid #d0d7de;font-size:12.5px;color:#57606a}
footer.bdrive a{color:inherit}
@media (prefers-color-scheme: dark){footer.bdrive{border-color:#3a3a44;color:#888}}
</style></head><body>%s
<footer class="bdrive">Shared with <a href="https://github.com/runbear-io/beardrive" rel="noopener">BearDrive</a> — synced files for AI agent teams</footer>
</body></html>`
