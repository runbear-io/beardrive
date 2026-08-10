package webapp

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// Share links make one file publicly readable at /s/<unguessable-token> —
// no sign-in needed, which is the whole point: "here's the report" is just a
// URL. A link is PINNED to the version it was published from and serves those
// bytes until someone with write access publishes a newer one (clicking Share
// again, or the banner's Publish button) or revokes it. A doc sent to a
// customer therefore cannot rewrite itself the next time an agent touches the
// file. Everything else on the hub stays behind auth.
//
// Links minted before pinning shipped carry an empty Sha and keep serving the
// latest synced content — the only reading that doesn't change the meaning of
// every URL already in the wild.
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
	// Sha/Size/Time are the published version: the blob this link serves, its
	// size (what quota bills), and when that version was synced. Empty Sha
	// means unpinned — a row written before pinning shipped — and serves the
	// latest content, exactly as it always did. All omitempty, so an old
	// shares.json round-trips byte-identical.
	Sha  string    `json:"sha,omitempty"`
	Size int64     `json:"size,omitempty"`
	Time time.Time `json:"published,omitzero"`
}

func (s Share) expired() bool {
	return !s.Expires.IsZero() && time.Now().After(s.Expires)
}

// ShareDB is the in-memory share registry over a MetaStore ShareRepo.
type ShareDB struct {
	repo ShareRepo

	mu      sync.Mutex
	ver     versionGate // skips the re-read when the store has not moved
	warned  bool        // "re-read failed" logged once (see refresh)
	byToken map[string]Share
}

// refresh re-reads the share registry from the store. Callers hold mu.
//
// fileShareRepo.reload closed the WRITE side of this in round 12 and its own
// comment names the row it did not close: a revoked link is gone from the file
// and still served by any hub process that did not handle the revocation, to
// anonymous strangers, for the life of that process. Revocation is the whole
// emergency stop for a leaked public URL, so the read that decides whether a
// /s/<token> is live has to read the store.
//
// A store that cannot answer leaves the map in place — see ProjectDB.refresh
// for the trade.
//
// ponytail: one full Load per share resolution; see OrgDB.refresh for the
// upgrade path if it ever shows up in a profile.
func (db *ShareDB) refresh() {
	token, stale := db.ver.stale(db.repo)
	if !stale {
		return
	}
	list, err := db.repo.Load()
	if err != nil {
		if !db.warned {
			db.warned = true
			log.Printf("beardrive: share registry re-read failed, serving the last known links: %v", err)
		}
		return
	}
	db.warned = false
	db.ver.fresh(token)
	next := make(map[string]Share, len(list))
	for _, s := range list {
		next[s.Token] = s
	}
	db.byToken = next
}

// NewShareDB builds the registry over a repo, loading its contents.
func NewShareDB(repo ShareRepo) (*ShareDB, error) {
	db := &ShareDB{repo: repo, byToken: make(map[string]Share)}
	list, err := repo.Load()
	if err != nil {
		return nil, err
	}
	for _, s := range list {
		db.byToken[s.Token] = s
	}
	return db, nil
}

// OpenShareDB loads the file-backed registry at path.
func OpenShareDB(path string) (*ShareDB, error) {
	return NewShareDB(newFileShareRepo(path))
}

// Create returns a share for (project, path), reusing an existing live one
// so repeated shares of the same file hand out the same URL — and, because a
// link is pinned, that reuse is what PUBLISHES: the second Share click on a
// live permanent link moves the pin to fi and keeps the token. fi is the
// file's current entry, which every caller already holds; a zero fi mints an
// unpinned (serve-latest) link.
func (db *ShareDB) Create(project, p, creator string, ttl time.Duration, fi FileInfo) (Share, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	for _, prev := range db.byToken {
		if prev.Project == project && prev.Path == p && !prev.expired() && prev.Expires.IsZero() && ttl == 0 {
			s := pin(prev, fi)
			db.byToken[s.Token] = s
			if err := db.repo.Put(s); err != nil {
				// Restore, don't delete: the row pre-existed, so dropping it
				// would revoke a live link over a disk hiccup (SetExpiry's
				// rollback, not Create's).
				db.byToken[prev.Token] = prev
				return Share{}, err
			}
			return s, nil
		}
	}
	s := pin(Share{
		Token: randHex(16), Project: project, Path: p,
		Creator: creator, Created: time.Now().UTC(),
	}, fi)
	if ttl > 0 {
		s.Expires = time.Now().UTC().Add(ttl)
	}
	db.byToken[s.Token] = s
	if err := db.repo.Put(s); err != nil {
		delete(db.byToken, s.Token)
		return Share{}, err
	}
	return s, nil
}

// pin stamps the published version onto a share. A zero fi (no blob) leaves
// the share unpinned rather than pinning it to nothing.
func pin(s Share, fi FileInfo) Share {
	if fi.Blob == "" {
		return s
	}
	s.Sha, s.Size, s.Time = fi.Blob, fi.Size, fi.Time
	return s
}

// Publish moves a live share's pin to fi, keeping the token — the URL on
// someone's clipboard keeps working and starts serving the new version. Same
// shape as SetExpiry, including its rollback.
func (db *ShareDB) Publish(token string, fi FileInfo) (Share, bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	prev, ok := db.byToken[token]
	if !ok || prev.expired() {
		return Share{}, false, nil
	}
	s := pin(prev, fi)
	db.byToken[token] = s
	if err := db.repo.Put(s); err != nil {
		db.byToken[token] = prev
		return Share{}, false, err
	}
	return s, true, nil
}

// Get resolves a live (non-expired) share.
func (db *ShareDB) Get(token string) (Share, bool) {
	if db == nil {
		return Share{}, false
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	s, ok := db.byToken[token]
	if !ok || s.expired() {
		return Share{}, false
	}
	return s, true
}

// lookup resolves a share regardless of expiry. Authorization must not depend
// on the clock: Get filters expired rows, so a handler that skips its
// permission check when the lookup misses lets anyone delete an expired
// share — and learn from the answer that it existed.
func (db *ShareDB) lookup(token string) (Share, bool) {
	if db == nil {
		return Share{}, false
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	s, ok := db.byToken[token]
	return s, ok
}

func (db *ShareDB) Revoke(token string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	sh, ok := db.byToken[token]
	if !ok {
		return false
	}
	delete(db.byToken, token)
	if err := db.repo.Delete(token); err != nil {
		// Revocation is the emergency stop for a leaked public URL: a delete
		// the store refused comes back at the next restart, so put the row
		// back and report the failure rather than reporting a revocation that
		// isn't one. Same shape as OrgDB.RevokeInvite.
		db.byToken[token] = sh
		return false
	}
	return true
}

// SetExpiry re-dates a live share in place: ttl > 0 sets the expiry, ttl == 0
// makes it permanent again. The token is untouched, so a URL already on
// someone's clipboard keeps working — which is the point, and why this is not
// revoke-and-remint.
func (db *ShareDB) SetExpiry(token string, ttl time.Duration) (Share, bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	prev, ok := db.byToken[token]
	if !ok || prev.expired() {
		return Share{}, false, nil
	}
	s := prev
	if ttl > 0 {
		s.Expires = time.Now().UTC().Add(ttl)
	} else {
		s.Expires = time.Time{}
	}
	db.byToken[token] = s
	if err := db.repo.Put(s); err != nil {
		// Restore, don't delete: unlike Create's rollback the row already
		// existed, and dropping it would revoke a live link over a disk hiccup.
		db.byToken[token] = prev
		return Share{}, false, err
	}
	return s, true, nil
}

// List returns a project's live shares, newest first. The order has to be a
// total one: byToken is a map, so without a sort every call reshuffles the
// rows — and these rows carry a Revoke button, so "the second one" must mean
// the same link on every load.
func (db *ShareDB) List(project string) []Share {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	var out []Share
	for _, s := range db.byToken {
		if s.Project == project && !s.expired() {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		// Equal, not !=: a time.Time carries a monotonic reading and a
		// location, so two logically-equal instants can compare unequal —
		// which would make this comparator non-transitive and leave the
		// order worse than the map iteration it replaces.
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Token < out[j].Token
	})
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
	fi, ok := snap.files[p]
	if !ok {
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
	// The snapshot entry is already in hand, so pinning costs no extra fetch.
	sh, err := s.Shares.Create(r.PathValue("project"), p, s.requestUser(r).Email, ttl, fi)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// No opens: a freshly minted link has nothing to report.
	writeJSON(w, shareJSON(r, sh, nil))
}

func (s *Server) handleShareList(v *volume, w http.ResponseWriter, r *http.Request) {
	if s.Shares == nil {
		http.Error(w, "sharing is not enabled on this server", http.StatusNotFound)
		return
	}
	project := r.PathValue("project")
	shares := s.Shares.List(project)
	opens := s.Reads.ShareOpens(project) // once, outside the loop — never per share
	// This is the only share route holding a *volume, so it is where "the file
	// moved on since you published" is answered. One snapshot for every share;
	// a snapshot the remote can't produce just drops the flags.
	var files map[string]FileInfo
	if snap, err := v.snapshot(r.Context()); err == nil {
		files = snap.files
	}
	out := make([]map[string]any, 0, len(shares))
	for _, sh := range shares {
		j := shareJSON(r, sh, opens)
		if sh.Sha != "" && files != nil {
			if fi, ok := files[sh.Path]; !ok {
				j["gone"] = true
			} else if fi.Blob != sh.Sha {
				j["stale"] = true
			}
		}
		out = append(out, j)
	}
	writeJSON(w, map[string]any{"shares": out})
}

func (s *Server) handleShareRevoke(w http.ResponseWriter, r *http.Request) {
	if s.Shares == nil {
		http.Error(w, "sharing is not enabled on this server", http.StatusNotFound)
		return
	}
	// This route is /api/shares/{token} — outside the proj() wrapper — so the
	// level check lives here: minting and killing public links are the same
	// authority.
	sh, ok := s.Shares.lookup(r.PathValue("token"))
	if !ok {
		http.Error(w, "no such share", http.StatusNotFound)
		return
	}
	if !s.requirePerm(w, r, sh.Project, PermWrite) {
		return
	}
	if s.Shares.Revoke(r.PathValue("token")) {
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	http.Error(w, "no such share", http.StatusNotFound)
}

// handleShareExpiry edits an existing link in place: it sets or clears the
// expiry, or (publish) moves the pin to the file's current version. Same shape
// and same authority as revoke — the token stays valid either way.
//
// Publish is a field here rather than its own route because Create's dedupe —
// the "clicking Share again republishes" path — only matches PERMANENT links
// (Expires.IsZero), so a second Share click on an expiring link mints a second
// token and orphans the pin the reader holds.
func (s *Server) handleShareExpiry(w http.ResponseWriter, r *http.Request) {
	if s.Shares == nil {
		http.Error(w, "sharing is not enabled on this server", http.StatusNotFound)
		return
	}
	var req struct {
		ExpiresIn string `json:"expires_in"` // Go duration, "" = permanent
		Publish   bool   `json:"publish"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	sh, ok := s.Shares.Get(r.PathValue("token"))
	if !ok {
		http.Error(w, "no such share", http.StatusNotFound)
		return
	}
	if !s.requirePerm(w, r, sh.Project, PermWrite) {
		return
	}
	if req.Publish {
		// Branch and RETURN: an absent expires_in decodes to "", and "" means
		// permanent, so falling through would silently clear the link's expiry.
		s.publishShare(w, r, sh)
		return
	}
	var ttl time.Duration
	if req.ExpiresIn != "" {
		var err error
		if ttl, err = time.ParseDuration(req.ExpiresIn); err != nil || ttl <= 0 {
			http.Error(w, "invalid expires_in", http.StatusBadRequest)
			return
		}
	}
	updated, ok, err := s.Shares.SetExpiry(sh.Token, ttl)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "no such share", http.StatusNotFound)
		return
	}
	// Same as create: an expiry edit is not the surface that reports opens.
	writeJSON(w, shareJSON(r, updated, nil))
}

// publishShare moves a link's pin to the file's current version. The caller
// has already checked PermWrite.
func (s *Server) publishShare(w http.ResponseWriter, r *http.Request, sh Share) {
	_, v, err := s.projectVolume(sh.Project)
	if err != nil {
		http.Error(w, "no such share", http.StatusNotFound)
		return
	}
	snap, err := v.snapshot(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Unlike the serve path, a missing file is fatal here: you cannot publish
	// what is not synced, and the link keeps serving what it already has.
	fi, ok := snap.files[sh.Path]
	if !ok {
		http.Error(w, fmt.Sprintf("%s is not synced to this project yet", sh.Path), http.StatusNotFound)
		return
	}
	updated, ok, err := s.Shares.Publish(sh.Token, fi)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "no such share", http.StatusNotFound)
		return
	}
	writeJSON(w, shareJSON(r, updated, nil))
}

// shareJSON renders one link. opens is the project's share-open map from
// ReadLedger.ShareOpens, built ONCE by the caller and indexed here — passing
// nil means "not measured" (reads are off, or this is a single-share reply
// that has nothing to report yet), and then neither receipt key appears.
// Absent is not zero: `0` would be a lie on a hub with reads disabled.
func shareJSON(r *http.Request, sh Share, opens map[string]ShareOpen) map[string]any {
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
	// The published VERSION, never the sha: a reader only needs to know when
	// this version went out and whether the file has moved on since (the
	// stale/gone flags handleShareList adds), and a content hash has no
	// business in a listing.
	if sh.Sha != "" {
		out["published"] = sh.Time
		out["size"] = sh.Size
	}
	if opens != nil {
		// Keyed by path, not token: heat has no token dimension, so two
		// links on one file report the same number. Documented, and asserted
		// in shares_test.go so it can't regress into a silent wrong answer.
		o := opens[sh.Path]
		out["opens"] = o.Count
		if o.Count > 0 {
			out["last_opened"] = o.Last
		}
	}
	return out
}

// shareCreatorStillBelongs reports whether the account that minted a link is
// still in the project's org. A share is the strongest grant on the hub — the
// org's live content, to anyone with the URL, forever — so offboarding has to
// reach it: the day someone leaves, every link they minted stops serving.
//
// Resolved at read time rather than by walking shares.json on RemoveMember,
// for the same reason projectPerm resolves membership instead of walking grant
// maps: one rule, no sweep to forget, and it self-heals if the account rejoins.
// Suspended, not deleted — an owner still sees the orphaned link in the
// project's share list and can revoke it for good. Demotion (write → read) is
// deliberately NOT covered: "a link lives until revoked" is the contract, and
// only leaving the org ends it.
func (s *Server) shareCreatorStillBelongs(sh Share) bool {
	if s.Dir == nil || sh.Creator == "" {
		return true // no membership model (single-volume), or a pre-accounts link
	}
	// No org on the project — cleared, never set, or the project is gone —
	// means membership cannot be established, and on a public route that is a
	// refusal, not a pass. Failing open here resurrected every link an
	// offboarded member ever minted, one layer below the same fix projectPerm
	// got in round 1.
	org := s.orgOf(sh.Project)
	if org == "" {
		return false
	}
	return s.Dir.Role(org, sh.Creator) != ""
}

// handleShared serves a share link: public, sandboxed, and the version that
// was published — not whatever the file says right now. A link minted before
// pinning shipped (empty Sha) still serves the latest synced content.
func (s *Server) handleShared(w http.ResponseWriter, r *http.Request) {
	// Sandbox everything under /s/ before anything can answer the request:
	// shared content executes in an opaque origin (scripts allowed — charts in
	// reports — but no cookies, no same-origin reach back into the hub), and
	// an error page is a /s/ response like any other. Set here, not at the
	// end, so the 429 and the 404s cannot go out bare.
	w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-popups")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")

	if !s.shareLimiter().allow(s.clientIP(r)) {
		http.Error(w, "too many requests — slow down", http.StatusTooManyRequests)
		return
	}
	sh, ok := s.Shares.Get(r.PathValue("token"))
	if !ok {
		http.Error(w, "this link does not exist or was revoked", http.StatusNotFound)
		return
	}
	if !s.shareCreatorStillBelongs(sh) {
		http.Error(w, "this link does not exist or was revoked", http.StatusNotFound)
		return
	}
	_, v, err := s.projectVolume(sh.Project)
	if err != nil {
		http.Error(w, "this link does not exist or was revoked", http.StatusNotFound)
		return
	}
	// A pinned link needs no snapshot at all: the blob is named on the share,
	// and surviving a rename or a delete of the path is half the point of
	// publishing. Only an unpinned (pre-pinning) link resolves by path.
	rs, _ := v.source.(*RemoteSource)
	pinned := sh.Sha != "" && rs != nil
	var fi FileInfo
	if pinned {
		fi = FileInfo{Blob: sh.Sha, Size: sh.Size, Time: sh.Time}
	} else {
		snap, err := v.snapshot(r.Context())
		if err != nil {
			http.Error(w, "content temporarily unavailable", http.StatusBadGateway)
			return
		}
		var ok bool
		if fi, ok = snap.files[sh.Path]; !ok {
			http.Error(w, "the shared file no longer exists", http.StatusNotFound)
			return
		}
	}
	// A share hit is external consumption. Actor is token+IP: one audience
	// member reloading is debounced to a visit, distinct visitors still count.
	s.Reads.Record(sh.Project, sh.Path, ReadKindShare, sh.Token+"/"+s.clientIP(r))

	// Share links are the only unauthenticated door to stored bytes, so they
	// are the only egress a plan actually caps. The per-IP limiter above
	// bounds REQUESTS; this bounds BYTES, which is the number a pricing page
	// can promise and a scraper behind many IPs can otherwise ignore.
	org := s.orgOf(sh.Project)
	if err := s.quota().CheckRead(org, fi.Size); err != nil {
		// Not "forbidden": nothing is wrong with the link or the reader, the
		// owner is over their transfer allowance. Say so plainly — whoever
		// opened this has no relationship with us and no way to fix it.
		http.Error(w, "This link has exceeded its transfer limit for now. "+
			"Ask whoever shared it to get in touch with us.", http.StatusTooManyRequests)
		return
	}
	// Count what actually leaves, on every branch below, including a transfer
	// the reader abandons halfway.
	cw := &countingWriter{w: w}
	defer func() { s.quota().RecordEgress(org, cw.n) }()

	// The published bytes, by content address — the identical call handleBlob
	// makes (history.go), and OpenBlob validates the sha itself, so a malformed
	// stored value is a 404 and never a traversal.
	//
	// This is the one thing pinning newly DEPENDS on: blobs are retained
	// forever (remote.Backend has no delete). A published link is a GC root
	// that nothing in the current file tree points at, so a future collector
	// that walks the live state and drops unreferenced blobs would break every
	// public URL on the hub, silently. Any such work has to treat live shares
	// as roots.
	open := func() (io.ReadCloser, error) { return rs.OpenBlob(r.Context(), sh.Sha) }
	if !pinned {
		open = func() (io.ReadCloser, error) { return v.source.Open(r.Context(), sh.Path, fi) }
	}
	rc, err := open()
	if err != nil {
		http.Error(w, "content temporarily unavailable", http.StatusBadGateway)
		return
	}
	defer rc.Close()

	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Type", contentType(sh.Path))
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", sanitizeFilename(path.Base(sh.Path))))
		io.Copy(cw, rc)
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
		fmt.Fprintf(cw, sharedMarkdownShell, html.EscapeString(path.Base(sh.Path)), updatedStamp(fi.Time), body)
	case ".html", ".htm":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.Copy(cw, rc)
	default:
		w.Header().Set("Content-Type", contentType(sh.Path))
		setContentLength(w, rc) // measured, never the journal's Size field
		io.Copy(cw, rc)
	}
}

// updatedStamp renders the "how old is this?" line a share page owes its
// reader — the published version has a date and the page has to show it (for a
// pinned link that is when it was published, not when the file last moved).
// Zero time (a source that doesn't know) prints nothing rather than 1970.
func updatedStamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	t = t.UTC()
	return fmt.Sprintf(`<div class="updated" title="%s">Last updated %s</div>`,
		html.EscapeString(t.Format(time.RFC3339)), html.EscapeString(t.Format("2 Jan 2006")))
}

// sharedMarkdownShell wraps rendered markdown in a minimal readable page.
// Verbs, in order: title, updated stamp, body.
const sharedMarkdownShell = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>%s</title>
<style>
body{font:16px/1.7 -apple-system,BlinkMacSystemFont,"SF Pro Text","Inter","Segoe UI",sans-serif;color:#24292f;
max-width:720px;margin:0 auto;padding:52px 24px 96px}
a{color:#b26a00}
h1,h2,h3{line-height:1.25;letter-spacing:-.018em}
pre{padding:12px;border-radius:8px;overflow-x:auto;background:#f6f8fa}
code{background:#f6f8fa;padding:2px 5px;border-radius:4px;font-size:.9em}
pre code{padding:0;background:none}
img{max-width:100%%}
blockquote{margin:0;padding-left:16px;border-left:3px solid #d0d7de;color:#57606a}
table{border-collapse:collapse;display:block;overflow-x:auto;max-width:100%%}td,th{border:1px solid #d0d7de;padding:5px 10px}
table.frontmatter{display:table;font-size:12px;color:#57606a;background:#f6f8fa;border-radius:8px;margin-bottom:24px}
table.frontmatter th,table.frontmatter td{border:none;border-bottom:1px solid #d8dee4;text-align:left;vertical-align:top}
table.frontmatter th{white-space:nowrap;color:#6e7781}
table.frontmatter tr:last-child th,table.frontmatter tr:last-child td{border-bottom:none}
table.frontmatter code{white-space:pre-wrap}
pre{max-width:100%%}
footer.bdrive{margin-top:64px;padding-top:14px;border-top:1px solid #d0d7de;font-size:12.5px;color:#57606a}
footer.bdrive a{color:inherit}
.updated{font-size:12.5px;color:#57606a;margin-bottom:28px}
/* Dark theme LAST: these rules sit at the same specificity as the light ones
   above, so source order is the whole fix — a dark block placed earlier loses
   to every light rule that follows it. Values are the hub's @theme tokens
   (frontend/src/tw.css), never hand-picked, so the two surfaces agree. */
@media (prefers-color-scheme: dark){
body{background:#0a0b0d;color:#eef0f3}
a{color:#ffcf85}
h1,h2,h3{color:#eef0f3}
pre,code{background:#15171b}
blockquote{border-left-color:rgba(255,255,255,.07);color:#9aa0a9}
td,th{border-color:rgba(255,255,255,.07)}
table.frontmatter{background:#15171b;color:#9aa0a9}
table.frontmatter th,table.frontmatter td{border-bottom-color:rgba(255,255,255,.07)}
table.frontmatter th{color:#868b93}
footer.bdrive{border-top-color:rgba(255,255,255,.07);color:#868b93}
.updated{color:#868b93}}
</style></head><body>%s%s
<footer class="bdrive">Shared with <a href="https://github.com/runbear-io/beardrive" rel="noopener">BearDrive</a> — synced files for AI agent teams</footer>
</body></html>`
