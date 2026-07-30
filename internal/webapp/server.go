// Package webapp serves the bdrive web server: a browsable web view of
// synced files (file tree reconstructed from the journals, rendered
// markdown, downloads), browser uploads, and — in hub mode — the sync API
// that lets storage-blind client devices sync whole projects through this
// server.
//
// Two modes:
//
//   - single-volume: Source is set (a DirSource for a plain folder, or a
//     RemoteSource in tests); the classic viewer.
//   - hub: Root + Projects are set; the server hosts many projects, each a
//     volume stored under <root>/<project-id>/ in the object store, managed
//     by a file-backed project registry.
//
// The client — browser or syncing device — is deliberately told nothing
// about the storage: no remote URL, bucket, or credentials ever appear in an
// API response.
package webapp

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"mime"
	"net/http"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

//go:embed static
var staticFiles embed.FS

// Source supplies the file set and content of one volume. Implementations:
// RemoteSource (a beardrive remote) and DirSource (a plain local folder).
type Source interface {
	Files(ctx context.Context) (map[string]FileInfo, error)
	Open(ctx context.Context, path string, fi FileInfo) (io.ReadCloser, error)
}

// Server renders volumes as a website and, in hub mode, brokers sync for
// client devices.
type Server struct {
	// Single-volume mode: serve exactly this source.
	Source Source
	Volume string // display only

	// Hub mode (when Root is set): many projects on one storage root.
	Root     remote.Backend
	Projects *ProjectDB

	// Device identifies this server in ops it journals for browser uploads.
	Device  Identity
	Refresh time.Duration
	Upload  UploadConfig
	// Auth, when set, gates the whole API behind sign-in. Nil means the
	// historical trusted-network behavior: no accounts, everyone welcome.
	Auth AuthProvider
	// Devices, when set, records what the server observes about syncing
	// devices (name, OS, public IP, last activity) for history.
	Devices *DeviceRegistry
	// Shares, when set, enables public share links (/s/<token>).
	Shares *ShareDB
	// Reads, when set, aggregates read telemetry (viewer, share, and agent
	// reads) for the heat API. Nil means read tracking is off.
	Reads *ReadLedger
	// Dir, when set, walls projects off by organization membership and owns
	// every org read and write the hub performs. LocalDirectory is the
	// built-in implementation; a managed deployment supplies its own so that
	// orgs come from the same place identities do. Nil means single-volume
	// mode: no orgs, every authenticated request passes.
	Dir Directory
	// Quota, when set, enforces plan limits (managed deployments). Nil
	// means UnlimitedQuota: the open-source server never says no.
	Quota QuotaProvider
	// Billing, when set, surfaces a billing entry in the frontend's account
	// menu: the billing page URL plus the signed-in user's current plan name
	// (/api/config `billing`). The OSS hub has no billing; managed
	// deployments plug this in. Nil — or ok=false for a user with no org —
	// hides the entry. The mirror of the Quota seam: Quota enforces the
	// plan, Billing displays it.
	Billing func(email string) (plan, url string, ok bool)
	// Analytics, when its Key is set, tells the frontend to load PostHog
	// (/api/config `analytics`). The third managed-deployment seam beside
	// Quota and Billing, and deliberately server-supplied rather than
	// bundled: with no key the OSS frontend ships no analytics code and
	// makes no third-party request, so a self-hosted hub cannot phone home
	// even by accident.
	Analytics AnalyticsConfig
	// ShareRPM is the per-IP request rate on public share links (/s/*);
	// 0 means DefaultShareRPM.
	ShareRPM int

	shareLimOnce sync.Once
	shareLim     *rateLimiter
	authLimOnce  sync.Once
	authLim      *rateLimiter

	volOnce sync.Once
	vol     *volume

	volsMu sync.Mutex
	vols   map[string]*volume // hub mode: per-project, keyed by project id
}

// UploadConfig controls whether and how clients may write.
type UploadConfig struct {
	Enabled bool
	// TTL bounds the lifetime of presigned direct-upload URLs.
	TTL time.Duration
}

// AnalyticsConfig points the frontend at a PostHog project. The key is a
// public write-only project token, not a credential — it is served to signed-
// out visitors too, because the app shell loads before login.
type AnalyticsConfig struct {
	Key  string // PostHog project key; empty disables analytics entirely
	Host string // PostHog API host; empty means DefaultAnalyticsHost
}

// DefaultAnalyticsHost is PostHog's US cloud ingestion host.
const DefaultAnalyticsHost = "https://us.i.posthog.com"

// Endpoint is Host with the default applied. Exported because the same
// config drives more than the app shell in a managed deployment (the cloud
// module's marketing pages render their own loader from it).
func (a AnalyticsConfig) Endpoint() string {
	if a.Host != "" {
		return a.Host
	}
	return DefaultAnalyticsHost
}

// DefaultUploadTTL is used when UploadConfig.TTL is unset: long enough for a
// slow upload, short enough that a leaked URL goes stale quickly.
const DefaultUploadTTL = 15 * time.Minute

func (c UploadConfig) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return DefaultUploadTTL
}

// FileInfo is the resolved state of one path: content identity (Blob doubles
// as the ETag), plus provenance where the source knows it.
type FileInfo struct {
	Blob string
	Size int64
	Time time.Time
	// User/UserName are the signed-in account behind the change; Author is
	// the git/OS identity an offline device falls back to. History renders
	// the account and falls back to Author, so the viewer needs all three
	// to give the same answer — see whoChanged() in the frontend.
	User     string
	UserName string
	Author   string
	Device   string
}

// volume is one browsable/syncable file set: a source plus its snapshot
// cache. File listings are cached for refresh between fetches; if the source
// becomes unreachable, the last good snapshot keeps being served.
type volume struct {
	source  Source
	refresh time.Duration

	mu   sync.Mutex
	snap *snapshot
	at   time.Time
}

type snapshot struct {
	files map[string]FileInfo
}

func (v *volume) snapshot(ctx context.Context) (*snapshot, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.snap != nil && time.Since(v.at) < v.refresh {
		return v.snap, nil
	}
	files, err := v.source.Files(ctx)
	if err != nil {
		if v.snap != nil {
			return v.snap, nil // serve stale rather than fail
		}
		return nil, err
	}
	v.snap, v.at = &snapshot{files: files}, time.Now()
	return v.snap, nil
}

// invalidate forces the next snapshot to refetch, so an upload shows up in
// the tree immediately instead of after refresh.
func (v *volume) invalidate() {
	v.mu.Lock()
	v.at = time.Time{}
	v.mu.Unlock()
}

func (v *volume) uploader() Uploader {
	u, _ := v.source.(Uploader)
	return u
}

// single returns the single-volume mode volume.
func (s *Server) single() *volume {
	s.volOnce.Do(func() {
		s.vol = &volume{source: s.Source, refresh: s.Refresh}
	})
	return s.vol
}

// projectVolume resolves a project id to its volume, creating the (cached)
// source over the project's storage prefix on first use.
func (s *Server) projectVolume(id string) (*volume, error) {
	if s.Root == nil || s.Projects == nil {
		return nil, fmt.Errorf("this server does not host projects")
	}
	if !projectIDRe.MatchString(id) {
		return nil, fmt.Errorf("invalid project id %q", id)
	}
	if _, ok := s.Projects.Get(id); !ok {
		return nil, fmt.Errorf("no such project %q", id)
	}
	s.volsMu.Lock()
	defer s.volsMu.Unlock()
	if s.vols == nil {
		s.vols = make(map[string]*volume)
	}
	v, ok := s.vols[id]
	if !ok {
		v = &volume{
			source:  &RemoteSource{Backend: remote.Prefixed(s.Root, id), Device: s.Device},
			refresh: s.Refresh,
		}
		s.vols[id] = v
	}
	return v, nil
}

// RemoteSource reads a beardrive remote: it fetches every journal and folds the
// ops into the current volume state (same total order as journal.Replay,
// but keeping author/device/time of the winning op per path). With Device set
// it also accepts uploads, journaled under that identity.
type RemoteSource struct {
	Backend remote.Backend
	// Device identifies this server in ops it journals for uploads. Required
	// for uploads; irrelevant for reading.
	Device Identity

	upmu sync.Mutex // serializes read-modify-write of our own journal
}

// Identity is the device identity uploads are journaled under.
type Identity struct {
	ID, Name, Author string
}

// loadOps fetches and parses every journal on the remote.
func (r *RemoteSource) loadOps(ctx context.Context) ([]journal.Op, error) {
	objs, err := r.Backend.List(ctx, "journal/")
	if err != nil {
		return nil, fmt.Errorf("list journals: %w", err)
	}
	var all []journal.Op
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".jsonl") {
			continue
		}
		rc, err := r.Backend.Get(ctx, o.Key)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", o.Key, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		ops, err := journal.Parse(data)
		if err != nil {
			continue // corrupt journal; ignore rather than break the view
		}
		all = append(all, ops...)
	}
	return all, nil
}

func (r *RemoteSource) Files(ctx context.Context) (map[string]FileInfo, error) {
	all, err := r.loadOps(ctx)
	if err != nil {
		return nil, err
	}
	journal.Sort(all)
	files := make(map[string]FileInfo)
	for _, op := range all {
		switch op.Kind {
		case journal.KindPut:
			files[op.Path] = FileInfo{
				Blob: op.Blob, Size: op.Size, Time: op.Time,
				User: op.User, UserName: op.UserName,
				Author: op.Author, Device: op.DeviceName,
			}
		case journal.KindDelete:
			delete(files, op.Path)
		}
	}
	return files, nil
}

func (r *RemoteSource) Open(ctx context.Context, _ string, fi FileInfo) (io.ReadCloser, error) {
	return r.Backend.Get(ctx, "blobs/"+fi.Blob)
}

// Handler returns the HTTP handler: /api/* plus the embedded frontend.
func (s *Server) Handler() http.Handler {
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err) // embedded FS; cannot fail at runtime
	}
	mux := http.NewServeMux()

	// Volume resolution per route family: fixed single volume, or by
	// project id in hub mode. One handler implementation serves both.
	// Single-volume mode has no per-project permissions, so it ignores the
	// declared level; hub mode enforces it.
	single := func(_ string, h func(*volume, http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if s.Source == nil {
				http.Error(w, "this server hosts projects; use /api/p/<project-id>/...", http.StatusNotFound)
				return
			}
			h(s.single(), w, r)
		}
	}
	proj := func(level string, h func(*volume, http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("project")
			v, err := s.projectVolume(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			if !s.requirePerm(w, r, id, level) {
				return
			}
			// Read recording (and anything else downstream) finds the project
			// id in the context; permission has already passed at this point.
			h(v, w, withProjectID(r, id))
		}
	}

	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/projects", s.handleProjectList)
	mux.HandleFunc("POST /api/projects", s.handleProjectCreate)
	mux.HandleFunc("GET /api/projects/{project}", s.handleProjectGet)

	for prefix, resolve := range map[string]func(string, func(*volume, http.ResponseWriter, *http.Request)) http.HandlerFunc{
		"/api/":             single,
		"/api/p/{project}/": proj,
	} {
		mux.HandleFunc("GET "+prefix+"tree", resolve(PermRead, s.handleTree))
		mux.HandleFunc("GET "+prefix+"file", resolve(PermRead, s.handleFile))
		mux.HandleFunc("GET "+prefix+"download", resolve(PermRead, s.handleDownload))
		mux.HandleFunc("GET "+prefix+"render", resolve(PermRead, s.handleRender))
		mux.HandleFunc("POST "+prefix+"upload/init", resolve(PermWrite, s.handleUploadInit))
		mux.HandleFunc("PUT "+prefix+"upload/content", resolve(PermWrite, s.handleUploadContent))
		mux.HandleFunc("POST "+prefix+"upload/commit", resolve(PermWrite, s.handleUploadCommit))
	}

	mux.HandleFunc("GET /api/orgs", s.handleOrgList)
	mux.HandleFunc("PATCH /api/orgs/{org}", s.handleOrgRename)
	mux.HandleFunc("POST /api/orgs/{org}/invites", s.handleInviteCreate)
	mux.HandleFunc("GET /api/orgs/{org}/invites", s.handleInviteList)
	mux.HandleFunc("DELETE /api/orgs/{org}/invites/{token}", s.handleInviteRevoke)
	mux.HandleFunc("PATCH /api/orgs/{org}/members/{email}", s.handleMemberUpdate)
	mux.HandleFunc("DELETE /api/orgs/{org}/members/{email}", s.handleMemberRemove)
	mux.HandleFunc("GET /api/orgs/{org}/shares", s.handleOrgShares)
	mux.HandleFunc("POST /api/invites/{token}", s.handleInviteAccept)

	mux.HandleFunc("PATCH /api/projects/{project}", s.handleProjectUpdate)
	mux.HandleFunc("DELETE /api/projects/{project}", s.handleProjectDelete)

	mux.HandleFunc("GET /api/admin/policy", s.handleAdminPolicy)
	mux.HandleFunc("POST /api/admin/policy", s.handleAdminPolicy)
	mux.HandleFunc("GET /api/admin/pending", s.handleAdminPending)
	mux.HandleFunc("POST /api/admin/pending/{id}/approve", s.handleAdminApprove)
	mux.HandleFunc("POST /api/admin/pending/{id}/deny", s.handleAdminDeny)

	mux.HandleFunc("GET /api/p/{project}/history", proj(PermRead, s.handleHistory))
	mux.HandleFunc("GET /api/p/{project}/blob", proj(PermRead, s.handleBlob))
	// Restore needs a journal to look the version up in, so it exists only
	// per project — never on the single-volume (DirSource) prefix.
	mux.HandleFunc("POST /api/p/{project}/restore", proj(PermWrite, s.handleRestore))
	mux.HandleFunc("GET /api/p/{project}/heat", proj(PermRead, s.handleHeat))
	mux.HandleFunc("POST /api/p/{project}/reads", proj(PermRead, s.handleReadReport))
	mux.HandleFunc("POST /api/p/{project}/shares", proj(PermWrite, s.handleShareCreate))
	mux.HandleFunc("GET /api/p/{project}/shares", proj(PermRead, s.handleShareList))
	mux.HandleFunc("PATCH /api/shares/{token}", s.handleShareExpiry)
	mux.HandleFunc("DELETE /api/shares/{token}", s.handleShareRevoke)
	mux.HandleFunc("GET /s/{token}", s.handleShared)

	mux.HandleFunc("GET /api/p/{project}/permissions", s.handleProjectPerms)
	mux.HandleFunc("PUT /api/p/{project}/permissions", s.handleProjectPermDefault)
	mux.HandleFunc("PUT /api/p/{project}/permissions/{email}", s.handleProjectPermSet)
	mux.HandleFunc("DELETE /api/p/{project}/permissions/{email}", s.handleProjectPermClear)

	// The sync (store) API only exists per project: hub mode is what
	// storage-blind devices sync through. Reading the store is how a
	// pull-only (read) device stays current; writing needs write.
	mux.HandleFunc("GET /api/p/{project}/store/list", proj(PermRead, s.handleStoreList))
	mux.HandleFunc("GET /api/p/{project}/store/object", proj(PermRead, s.handleStoreGet))
	mux.HandleFunc("GET /api/p/{project}/store/exists", proj(PermRead, s.handleStoreExists))
	mux.HandleFunc("POST /api/p/{project}/store/sign", proj(PermWrite, s.handleStoreSign))
	mux.HandleFunc("PUT /api/p/{project}/store/object", proj(PermWrite, s.handleStorePut))

	mux.Handle("GET /", s.frontend(static))
	if s.Auth != nil {
		s.Auth.Register(mux)
	}
	return s.rateLimitAuth(s.authGate(mux))
}

// frontend serves the embedded single-page app. Real asset files (app.js,
// style.css) are served directly; every other GET that isn't an API, auth,
// or share route returns index.html, so client-side routes like
// /<project-id>/<path> and /join/<token> survive a deep link or refresh.
func (s *Server) frontend(static fs.FS) http.HandlerFunc {
	files := http.FileServerFS(static)
	index, _ := fs.ReadFile(static, "index.html")
	return func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		// Vite emits content-hashed filenames under assets/, safe to cache
		// forever. Everything else (index.html above all) must revalidate:
		// embedded files carry no modtime, so without no-cache browsers
		// cache heuristically and users see a stale frontend after upgrades.
		if strings.HasPrefix(upath, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		// Reserved prefixes that fell through to the catch-all are genuine
		// 404s — don't mask a mistyped API/auth/share URL with the app shell.
		if strings.HasPrefix(upath, "api/") || strings.HasPrefix(upath, "auth/") || strings.HasPrefix(upath, "s/") {
			http.NotFound(w, r)
			return
		}
		// A hub whose organizations live elsewhere has no org page to show:
		// send the browser where they are actually administered rather than
		// painting a console whose every control would 409. The account menu
		// already links to the same place; this covers bookmarks, history, and
		// hand-typed URLs, which are the paths a link cannot reach.
		if id, ok := strings.CutPrefix(upath, "orgs/"); ok && s.Dir != nil {
			if u := s.Dir.ManageURL(id); !strings.HasPrefix(u, "/") {
				http.Redirect(w, r, u, http.StatusFound)
				return
			}
		}
		if upath != "" && upath != "index.html" {
			if f, err := static.Open(upath); err == nil {
				fi, statErr := f.Stat()
				f.Close()
				if statErr == nil && !fi.IsDir() {
					files.ServeHTTP(w, r) // a real asset
					return
				}
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	}
}

// handleConfig tells the client how this server is configured. Deliberately
// nothing about the storage backend.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	mode := "volume"
	if s.Root != nil {
		mode = "hub"
	}
	auth := map[string]any{"enabled": s.Auth != nil}
	if s.Auth != nil {
		auth["cli_login"] = s.Auth.CLILoginPath()
	}
	// Tell the frontend whether self-signup is offered and whether the
	// signed-in user is a hub admin, so it can hide the "Sign up" link and
	// show the admin surfaces. Never leak more than these booleans.
	me := s.requestUser(r)
	brand := ""
	if a, ok := s.Auth.(AccountApprover); ok {
		// Only a hub that owns its accounts can offer self-signup or an admin
		// queue; one whose identities come from elsewhere offers neither.
		auth["allow_signup"] = a.Policy().AllowSignup
		auth["admin"] = me.Admin
	}
	if b, ok := s.Auth.(Brander); ok {
		brand = b.Branding()
	}
	// No fallback: the volume is a storage basename, not a brand. An
	// unconfigured brand stays empty and each app picks its own default
	// (hub: "BearDrive", volume mode: the folder name).
	out := map[string]any{
		"mode":   mode,
		"volume": s.Volume,
		"brand":  brand,
		"upload": map[string]any{
			"enabled": s.Upload.Enabled,
		},
		"auth":  auth,
		"reads": map[string]any{"enabled": s.Reads != nil},
	}
	// Outside a managed deployment this block is absent and the frontend
	// never loads a tracker. Outside the `me` check on purpose: a hub with
	// auth off has no signed-in user and should still be measurable.
	// Note the funnel gap this leaves — /auth/* is server-rendered HTML
	// (authlocal.go authPage) with no analytics, so a visitor is counted on
	// the marketing page and again once the app boots, but the signup page
	// itself reports nothing. Same origin means the anonymous id survives
	// the round trip, so attribution holds; only signup-page drop-off is
	// invisible. Wire authPage up if that becomes the question.
	if s.Analytics.Key != "" {
		out["analytics"] = map[string]string{"key": s.Analytics.Key, "host": s.Analytics.Endpoint()}
	}
	if me.Email != "" {
		out["me"] = map[string]string{"email": me.Email, "name": me.Name}
		if s.Billing != nil {
			if plan, url, ok := s.Billing(me.Email); ok {
				out["billing"] = map[string]string{"plan": plan, "url": url}
			}
		}
	}
	writeJSON(w, out)
}

func (s *Server) handleProjectList(w http.ResponseWriter, r *http.Request) {
	if s.Projects == nil {
		http.Error(w, "this server does not host projects", http.StatusNotFound)
		return
	}
	// Each row carries the caller's own level, so the frontend can hide write
	// affordances without a second fetch per project on every render.
	visible := []projectView{}
	for _, p := range s.Projects.List() {
		perm := s.projectPerm(r, p.ID)
		if !atLeast(perm, PermRead) {
			continue
		}
		visible = append(visible, projectJSON(p, perm))
	}
	writeJSON(w, map[string]any{"projects": visible})
}

// projectJSON renders a project for the API with the caller's effective level.
// projectView is a Project plus the caller's own effective level on it.
// It embeds rather than re-listing fields on purpose: hand-listing them means
// every new Project field silently fails to reach the client until someone
// remembers to add it here.
type projectView struct {
	Project
	Perm string `json:"perm"`
}

func projectJSON(p Project, perm string) projectView {
	// The grant list and the default belong to /api/p/{id}/permissions, which
	// has its own gate; they'd be noise on every row of every project list.
	p.Perms, p.Default = nil, ""
	return projectView{p, perm}
}

func (s *Server) handleProjectGet(w http.ResponseWriter, r *http.Request) {
	if s.Projects == nil {
		http.Error(w, "this server does not host projects", http.StatusNotFound)
		return
	}
	p, ok := s.Projects.Get(r.PathValue("project"))
	perm := s.projectPerm(r, p.ID)
	if !ok || !atLeast(perm, PermRead) {
		http.Error(w, "no such project", http.StatusNotFound)
		return
	}
	writeJSON(w, projectJSON(p, perm))
}

// handleProjectCreate creates a project by name, or returns the existing one
// with that name (create-or-join). Creating is a write, so it follows the
// upload setting.
func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	if s.Projects == nil {
		http.Error(w, "this server does not host projects", http.StatusNotFound)
		return
	}
	if !s.Upload.Enabled {
		http.Error(w, "this server is read-only; projects cannot be created", http.StatusForbidden)
		return
	}
	var req struct {
		Name string `json:"name"`
		Org  string `json:"org,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	org, err := s.orgForCreate(r, req.Org)
	if err != nil {
		if errors.Is(err, ErrManagedElsewhere) {
			// A user with no organization on a hub that cannot create one:
			// send them where organizations actually come from, rather than a
			// 403 naming an org that does not exist.
			s.writeDirErr(w, "", err)
			return
		}
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	p, created, err := s.Projects.GetOrCreate(req.Name, org)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if created {
		// The creator is the project's first admin. Both writes are
		// best-effort in the sense that a failure leaves a usable project
		// governed by org owners — but report it rather than lie.
		me := normEmail(s.requestUser(r).Email)
		if me != "" {
			if err := s.Projects.SetCreator(p.ID, me); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			// An org owner is already implicitly admin; an explicit grant on
			// one is refused elsewhere, so don't write one here either.
			if s.Dir == nil || org == "" || s.Dir.Role(org, me) != RoleOwner {
				if err := s.Projects.SetPerm(p.ID, me, PermAdmin); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			p, _ = s.Projects.Get(p.ID)
		}
	} else if !atLeast(s.projectPerm(r, p.ID), PermRead) {
		// GetOrCreate is create-or-join by name: without this, POSTing the
		// name of a project you've been cut off from would hand back its id.
		http.Error(w, permDenied(PermRead), http.StatusForbidden)
		return
	}
	writeJSON(w, map[string]any{"project": projectJSON(p, s.projectPerm(r, p.ID)), "created": created})
}

// orgForCreate resolves which org a new project lands in: the explicitly
// requested one (must be a membership), else the caller's only org, else —
// for an account in no org yet — a fresh org named after the account, so
// nobody is ever blocked from starting to sync. Orgs disabled → "".
func (s *Server) orgForCreate(r *http.Request, requested string) (string, error) {
	if s.Dir == nil || s.Auth == nil {
		return "", nil
	}
	me := s.requestUser(r)
	if requested != "" {
		if s.Dir.Role(requested, me.Email) == "" {
			return "", fmt.Errorf("you are not a member of organization %q", requested)
		}
		return requested, nil
	}
	mine := s.Dir.OrgsFor(me.Email)
	if len(mine) > 0 {
		return mine[0].ID, nil
	}
	name := me.Name
	if name == "" {
		name = strings.SplitN(me.Email, "@", 2)[0]
	}
	o, err := s.Dir.Create(name, me.Email)
	if err != nil {
		return "", err
	}
	return o.ID, nil
}

// Node is one entry of the file tree returned by the tree endpoint.
type Node struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Dir      bool      `json:"dir"`
	Size     int64     `json:"size,omitempty"`
	Time time.Time `json:"time,omitzero"`
	// Same three-field "who" shape as HistoryEntry (history.go), so the
	// frontend has one attribution helper for every surface.
	User     string  `json:"user,omitempty"`
	UserName string  `json:"user_name,omitempty"`
	Author   string  `json:"author,omitempty"`
	Device   string  `json:"device,omitempty"`
	Children []*Node `json:"children,omitempty"`
}

func (s *Server) handleTree(v *volume, w http.ResponseWriter, r *http.Request) {
	snap, err := v.snapshot(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, buildTree(snap.files))
}

func buildTree(files map[string]FileInfo) *Node {
	root := &Node{Name: "/", Dir: true}
	dirs := map[string]*Node{"": root}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		fi := files[p]
		parent := root
		segs := strings.Split(p, "/")
		for i := 0; i < len(segs)-1; i++ {
			dp := strings.Join(segs[:i+1], "/")
			n, ok := dirs[dp]
			if !ok {
				n = &Node{Name: segs[i], Path: dp, Dir: true}
				dirs[dp] = n
				parent.Children = append(parent.Children, n)
			}
			parent = n
		}
		parent.Children = append(parent.Children, &Node{
			Name: segs[len(segs)-1], Path: p,
			Size: fi.Size, Time: fi.Time,
			User: fi.User, UserName: fi.UserName, Author: fi.Author, Device: fi.Device,
		})
	}
	sortTree(root)
	return root
}

func sortTree(n *Node) {
	sort.SliceStable(n.Children, func(i, j int) bool {
		a, b := n.Children[i], n.Children[j]
		if a.Dir != b.Dir {
			return a.Dir // folders first, like Obsidian
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	for _, c := range n.Children {
		if c.Dir {
			sortTree(c)
		}
	}
}

// lookup resolves ?path= against the volume's current snapshot.
func lookup(v *volume, r *http.Request) (string, FileInfo, int, error) {
	p := r.URL.Query().Get("path")
	if p == "" {
		return "", FileInfo{}, http.StatusBadRequest, fmt.Errorf("missing ?path=")
	}
	snap, err := v.snapshot(r.Context())
	if err != nil {
		return "", FileInfo{}, http.StatusBadGateway, err
	}
	fi, ok := snap.files[p]
	if !ok {
		return "", FileInfo{}, http.StatusNotFound, fmt.Errorf("no such file: %s", p)
	}
	return p, fi, 0, nil
}

func (s *Server) serveBlob(v *volume, w http.ResponseWriter, r *http.Request, attach bool) {
	p, fi, code, err := lookup(v, r)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	// Count the read before the ETag check: a 304 render is still a person
	// reading the file, and skipping it would undercount the hottest pages.
	s.recordRead(r, p)
	etag := `"` + fi.Blob + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	rc, err := v.source.Open(r.Context(), p, fi)
	if err != nil {
		http.Error(w, fmt.Sprintf("fetch content: %v", err), http.StatusBadGateway)
		return
	}
	defer rc.Close()
	w.Header().Set("ETag", etag)
	ct := contentType(p)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", fmt.Sprint(fi.Size))
	if attach {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(p)))
	} else if strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "image/svg") {
		// Synced HTML (and scriptable SVG) served inline must never run
		// with the hub origin's session — same posture as /s/* share
		// pages: an opaque sandboxed origin that can't touch the API or
		// cookies. The viewer renders these in a sandboxed iframe; direct
		// navigation gets the same wall.
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts")
	}
	io.Copy(w, rc)
}

func (s *Server) handleFile(v *volume, w http.ResponseWriter, r *http.Request) {
	s.serveBlob(v, w, r, false)
}

func (s *Server) handleDownload(v *volume, w http.ResponseWriter, r *http.Request) {
	s.serveBlob(v, w, r, true)
}

func (s *Server) handleRender(v *volume, w http.ResponseWriter, r *http.Request) {
	if sha := r.URL.Query().Get("sha"); sha != "" {
		s.renderVersion(v, w, r, sha)
		return
	}
	p, fi, code, err := lookup(v, r)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	s.recordRead(r, p)
	rc, err := v.source.Open(r.Context(), p, fi)
	if err != nil {
		http.Error(w, fmt.Sprintf("fetch content: %v", err), http.StatusBadGateway)
		return
	}
	src, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	html, err := RenderMarkdown(src)
	if err != nil {
		http.Error(w, fmt.Sprintf("render: %v", err), http.StatusInternalServerError)
		return
	}
	doc := map[string]any{
		"path": p, "html": html,
		"size": fi.Size, "time": fi.Time, "author": fi.Author, "device": fi.Device,
	}
	// Omitted rather than sent empty, so a journal from before accounts
	// existed still renders its Author instead of a blank attribution.
	if fi.User != "" {
		doc["user"] = fi.User
	}
	if fi.UserName != "" {
		doc["user_name"] = fi.UserName
	}
	writeJSON(w, doc)
}

// renderVersion renders one exact past version by content hash — the
// markdown counterpart of /blob?sha=, so opening an old .md from history
// shows a rendered page instead of raw source. Provenance is not returned:
// the caller already has the history entry it clicked. Viewing history is
// never a read (see the read-heat invariant), so nothing is recorded.
func (s *Server) renderVersion(v *volume, w http.ResponseWriter, r *http.Request, sha string) {
	if !blobRe.MatchString(sha) {
		http.Error(w, "invalid sha", http.StatusBadRequest)
		return
	}
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	rc, err := rs.Backend.Get(r.Context(), "blobs/"+sha)
	if err != nil {
		http.Error(w, "no such version", http.StatusNotFound)
		return
	}
	src, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	html, err := RenderMarkdown(src)
	if err != nil {
		http.Error(w, fmt.Sprintf("render: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"path": r.URL.Query().Get("path"), "html": html, "size": len(src),
	})
}

func contentType(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".txt", ".log", ".go", ".py", ".js", ".ts", ".sh", ".yaml", ".yml", ".toml", ".csv":
		return "text/plain; charset=utf-8"
	case ".json":
		return "application/json"
	}
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
